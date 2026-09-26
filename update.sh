#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
blue='\033[0;34m'
yellow='\033[0;33m'
plain='\033[0m'

xui_folder="${XUI_MAIN_FOLDER:=/usr/local/x-ui}"
xui_service="${XUI_SERVICE:=/etc/systemd/system}"

# Resolve the directory the script lives in. When the script is piped via
# `bash <(curl ...)` this resolves to /dev/fd/N — the local-source detector
# below will then find no source files and fall back to GitHub.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)" || SCRIPT_DIR=""

# Returns 0 when update.sh is being run from inside a cloned 3ax-ui git
# checkout. Mirrors install.sh — see comment there for the BASH_SOURCE
# safety check.
is_local_source_install() {
    local src_name
    src_name="$(basename "${BASH_SOURCE[0]:-}")"
    [[ "$src_name" == "update.sh" ]] || return 1
    [[ -n "$SCRIPT_DIR" ]] || return 1
    [[ -f "$SCRIPT_DIR/update.sh" ]] || return 1
    [[ -f "$SCRIPT_DIR/main.go" ]] || return 1
    [[ -f "$SCRIPT_DIR/go.mod" ]] || return 1
    [[ -d "$SCRIPT_DIR/web" ]] || return 1
    [[ -d "$SCRIPT_DIR/.git" ]] || return 1
    return 0
}

# Branch to fetch auxiliary files (x-ui.sh, service files) from.
#
# Always `main`. `--beta`/`--pre` choose which *release* to install, not which
# branch the helper files come from: this fork has no `dev` branch, so the old
# mapping sent every --beta run to a 404 — and the wrapper fetch that 404'd sat
# below the point where the service is stopped and its unit removed, which is
# how a --beta update left a front on the stand with no service at all.
# XUI_REPO_BRANCH overrides it for testing from a branch.
REPO_BRANCH="${XUI_REPO_BRANCH:-main}"

# GitHub repo (owner/name) to fetch the release binary, wrapper and service
# files from. Override with XUI_REPO=owner/name to update from a fork.
XUI_REPO="${XUI_REPO:-SBKubric/sane-3x-ui}"

# Don't edit this config
b_source="${BASH_SOURCE[0]}"
while [ -h "$b_source" ]; do
    b_dir="$(cd -P "$(dirname "$b_source")" >/dev/null 2>&1 && pwd || pwd -P)"
    b_source="$(readlink "$b_source")"
    [[ $b_source != /* ]] && b_source="$b_dir/$b_source"
done
cur_dir="$(cd -P "$(dirname "$b_source")" >/dev/null 2>&1 && pwd || pwd -P)"
script_name=$(basename "$0")

# Check command exist function
_command_exists() {
    type "$1" &>/dev/null
}

# Fail, log and exit script function
_fail() {
    local msg=${1}
    echo -e "${red}${msg}${plain}"
    exit 2
}

# _fail_after_stop <msg> — a failure once the service has already been stopped
# and its unit removed.
#
# Plain _fail is safe only while nothing has been touched. Past the stop it
# leaves the box with no service at all: no unit, disabled, nothing for
# systemctl or the next update to restart. So put the unit back first and then
# report — a unit pointing at a half-installed folder is still recoverable,
# a box with no unit needs someone to ssh in.
_fail_after_stop() {
    local msg=${1}
    echo -e "${red}${msg}${plain}"
    echo -e "${yellow}Reinstalling the service unit so this box keeps one...${plain}"
    update_x-ui_install_service
    exit 2
}

# check root
[[ $EUID -ne 0 ]] && _fail "FATAL ERROR: Please run this script with root privilege."

if _command_exists curl; then
    curl_bin=$(which curl)
else
    _fail "ERROR: Command 'curl' not found."
fi

# Check OS and set release variable
if [[ -f /etc/os-release ]]; then
    source /etc/os-release
    release=$ID
    elif [[ -f /usr/lib/os-release ]]; then
    source /usr/lib/os-release
    release=$ID
else
    _fail "Failed to check the system OS, please contact the author!"
fi
echo "The OS release is: $release"

arch() {
    case "$(uname -m)" in
        x86_64 | x64 | amd64) echo 'amd64' ;;
        i*86 | x86) echo '386' ;;
        armv8* | armv8 | arm64 | aarch64) echo 'arm64' ;;
        armv7* | armv7 | arm) echo 'armv7' ;;
        armv6* | armv6) echo 'armv6' ;;
        armv5* | armv5) echo 'armv5' ;;
        s390x) echo 's390x' ;;
        *) echo -e "${red}Unsupported CPU architecture!${plain}" && rm -f "${cur_dir}/${script_name}" >/dev/null 2>&1 && exit 2;;
    esac
}

echo "Arch: $(arch)"

# Simple helpers
is_ipv4() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] && return 0 || return 1
}
is_ipv6() {
    [[ "$1" =~ : ]] && return 0 || return 1
}
is_ip() {
    is_ipv4 "$1" || is_ipv6 "$1"
}
is_domain() {
    [[ "$1" =~ ^([A-Za-z0-9](-*[A-Za-z0-9])*\.)+(xn--[a-z0-9]{2,}|[A-Za-z]{2,})$ ]] && return 0 || return 1
}

# List certificate identifiers known to acme.sh (main domain of each cert)
acme_cert_domains() {
    [[ -f ~/.acme.sh/acme.sh ]] || return 1
    ~/.acme.sh/acme.sh --list 2>/dev/null | awk 'NR>1 && NF && $1 != "Main_Domain" {print $1}'
}

# Port helpers
is_port_in_use() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | awk -v p=":${port}$" '$4 ~ p {exit 0} END {exit 1}'
        return
    fi
    if command -v netstat >/dev/null 2>&1; then
        netstat -lnt 2>/dev/null | awk -v p=":${port} " '$4 ~ p {exit 0} END {exit 1}'
        return
    fi
    if command -v lsof >/dev/null 2>&1; then
        lsof -nP -iTCP:${port} -sTCP:LISTEN >/dev/null 2>&1 && return 0
    fi
    return 1
}

gen_random_string() {
    local length="$1"
    local random_string=$(LC_ALL=C tr -dc 'a-zA-Z0-9' </dev/urandom | fold -w "$length" | head -n 1)
    echo "$random_string"
}

# Returns 0 if the URL host responds within a short timeout, non-zero on
# connection / DNS / TLS failure. HEAD-only so we don't pull the full
# asset just to probe. No -f: a 404 still means the network path works.
url_reachable() {
    ${curl_bin} --connect-timeout 5 --max-time 10 -sSIL -o /dev/null "$1" 2>/dev/null
}

# Probes URL reachability before downloading. If unreachable, names the
# broken URL and asks the user whether to continue without that resource
# (default Y = skip and proceed). Aborts the script on N.
check_url_or_skip() {
    local url="$1"
    local label="$2"
    if url_reachable "$url"; then
        return 0
    fi
    echo ""
    echo -e "${yellow}══════════════════════════════════════════════════════${plain}"
    echo -e "${yellow}  Failed to reach: ${url}${plain}"
    echo -e "${yellow}  Module / file:   ${label}${plain}"
    echo -e "${yellow}══════════════════════════════════════════════════════${plain}"
    read -rp "Continue without it? [Y/n]: " __skip_choice
    case "${__skip_choice,,}" in
        n|no)
            echo -e "${red}Aborted by user.${plain}"
            exit 1
            ;;
        *)
            echo -e "${yellow}Skipping ${label}.${plain}"
            return 1
            ;;
    esac
}

install_base() {
    echo -e "${green}Updating and install dependency packages...${plain}"
    case "${release}" in
        ubuntu | debian | armbian)
            apt-get update >/dev/null 2>&1 && apt-get install -y -q curl tar tzdata socat >/dev/null 2>&1
        ;;
        fedora | amzn | virtuozzo | rhel | almalinux | rocky | ol)
            dnf -y update >/dev/null 2>&1 && dnf install -y -q curl tar tzdata socat >/dev/null 2>&1
        ;;
        centos)
            if [[ "${VERSION_ID}" =~ ^7 ]]; then
                yum -y update >/dev/null 2>&1 && yum install -y -q curl tar tzdata socat >/dev/null 2>&1
            else
                dnf -y update >/dev/null 2>&1 && dnf install -y -q curl tar tzdata socat >/dev/null 2>&1
            fi
        ;;
        arch | manjaro | parch)
            pacman -Syu >/dev/null 2>&1 && pacman -Syu --noconfirm curl tar tzdata socat >/dev/null 2>&1
        ;;
        opensuse-tumbleweed | opensuse-leap)
            zypper refresh >/dev/null 2>&1 && zypper -q install -y curl tar timezone socat >/dev/null 2>&1
        ;;
        alpine)
            apk update >/dev/null 2>&1 && apk add curl tar tzdata socat >/dev/null 2>&1
        ;;
        *)
            apt-get update >/dev/null 2>&1 && apt install -y -q curl tar tzdata socat >/dev/null 2>&1
        ;;
    esac
}

install_acme() {
    echo -e "${green}Installing acme.sh for SSL certificate management...${plain}"
    cd ~ || return 1
    curl -s https://get.acme.sh | sh >/dev/null 2>&1
    if [ $? -ne 0 ]; then
        echo -e "${red}Failed to install acme.sh${plain}"
        return 1
    else
        echo -e "${green}acme.sh installed successfully${plain}"
    fi
    return 0
}

# acme_ip_flags pins acme.sh to one IP family: IPv4 unless XUI_TLS_IPV6=1 (or
# PROXY_TLS_IPV6=1). A cold dual-stack connect to the CA on a box without a
# working IPv6 path costs curl its whole connect timeout, and acme.sh then gives
# up with `Cannot init API`. The full story is beside the same function in
# install.sh.
acme_ip_flags() {
    if [[ "${PROXY_TLS_IPV6:-${XUI_TLS_IPV6:-}}" == "1" ]]; then
        echo "--listen-v6"
        return
    fi
    echo "--listen-v4 --request-v4"
}

# acme_webroot prints where acme.sh drops the HTTP-01 challenge and nginx
# serves it from on port 80 — nginx.ACMEWebroot in the binary.
#
# The certificate helpers from here to acme_issue_webroot are the same text in
# install.sh, update.sh and x-ui.sh, and so is acme_ip_flags: each script runs
# on its own, and one issuing path for the panel and the hops means one text.
# install_acme_webroot_test.go keeps the copies, and this path, in step.
acme_webroot() {
    echo "/usr/local/x-ui/acme-webroot"
}

# acme_nginx_reload_cmd makes nginx re-read a renewed certificate: the HTTP
# side of the front serves the files acme.sh has just replaced.
acme_nginx_reload_cmd() {
    echo "systemctl reload nginx 2>/dev/null || nginx -s reload 2>/dev/null"
}

# acme_reload_cmd is the --reloadcmd of every certificate the scripts issue:
# nginx reloads, and x-ui restarts, because the panel — or a hop's sub port —
# serves the same files. "|| true" because the x-ui service may not exist yet
# during a first install, and acme.sh reports a failed reloadcmd as a failure.
acme_reload_cmd() {
    echo "$(acme_nginx_reload_cmd); systemctl restart x-ui 2>/dev/null || rc-service x-ui restart 2>/dev/null || true"
}

# acme_front_ready puts nginx on port 80 in front of the ACME webroot.
#
# Port 80 belongs to nginx on every box (ADR 0005). acme.sh used to listen
# there itself in standalone mode and lost the port to any nginx already
# running — the distro's default site included — so issuance and renewal
# failed on exactly the boxes that run the front. Now acme.sh only writes the
# challenge file and nginx answers the CA.
#
# Once acme_front_setup has failed in this run it stays failed: that function
# may have stopped an nginx the run installed, and starting it again here
# would put the distro's default site back in front of the standalone path.
acme_front_ready() {
    if [[ "${acme_front_failed:-0}" == "1" ]]; then
        echo -e "${yellow}nginx could not take port 80 earlier in this run — not issuing through it.${plain}"
        return 1
    fi
    if ! command -v nginx >/dev/null 2>&1; then
        install_nginx
    fi
    if ! command -v nginx >/dev/null 2>&1; then
        echo -e "${red}nginx is not installed, and it is nginx that answers the CA on port 80 — no certificate can be issued.${plain}"
        return 1
    fi
    if ! "${xui_folder}/x-ui" nginx acme-front; then
        echo -e "${red}nginx could not take port 80 for the ACME challenge (see above).${plain}"
        return 1
    fi
}

# acme_issue_webroot <cert-dir> <reloadcmd> <name>... issues a Let's Encrypt
# certificate for the names through nginx's webroot on port 80 and installs it
# as <cert-dir>/fullchain.pem and <cert-dir>/privkey.pem.
#
# The one issuing path of the panel and the hops, for addresses and domains
# alike. An address gets the shortlived profile (~160 h) renewed every 3 days,
# around its half-life, so a daily cron that misses a run still has days in
# hand; a domain gets acme.sh's defaults. An empty <reloadcmd> means
# acme_reload_cmd.
#
# Non-zero when the certificate could not be had, and then only what this
# attempt created is removed: a failed re-issue must not take the working
# certificate, or acme.sh's record that renews it, with it.
acme_issue_webroot() {
    local __dir="$1" __reload="$2"
    shift 2
    local __name __d __all_ip=1 __try __ok=0 __rc __log __dir_existed=0
    local -a __args=() __created=()
    for __name in "$@"; do
        __args+=(-d "${__name}")
        is_ip "${__name}" || __all_ip=0
        for __d in "${HOME}/.acme.sh/${__name}" "${HOME}/.acme.sh/${__name}_ecc"; do
            [[ -e "${__d}" ]] || __created+=("${__d}")
        done
    done
    [[ ${__all_ip} -eq 1 ]] && __args+=(--certificate-profile shortlived --days 3)
    [[ -z "${__reload}" ]] && __reload="$(acme_reload_cmd)"
    [[ -e "${__dir}" ]] && __dir_existed=1

    acme_front_ready || return 1
    mkdir -p "${__dir}"
    ~/.acme.sh/acme.sh --set-default-ca --server letsencrypt --force >/dev/null 2>&1
    __log=$(mktemp)
    for __try in 1 2; do
        # shellcheck disable=SC2046 # acme_ip_flags returns two flags on purpose
        ~/.acme.sh/acme.sh --issue "${__args[@]}" --webroot "$(acme_webroot)" \
            --server letsencrypt $(acme_ip_flags) --force 2>&1 | tee "${__log}"
        __rc=${PIPESTATUS[0]}
        if [[ ${__rc} -eq 0 ]]; then
            __ok=1
            break
        fi
        # The CA answered and said no: the challenge could not be fetched
        # from port 80, or a limit was hit. Asking again would only spend
        # another of its failed-validation allowances.
        if grep -qE 'urn:ietf:params:acme:error|Invalid status|Verify error|Verification error|rateLimited' "${__log}"; then
            echo -e "${red}Validation failed: the CA could not fetch the challenge through port 80, or refused the order (see above). Not retrying.${plain}"
            break
        fi
        # The CA did not answer at all — a cold dual-stack connect eating
        # curl's timeout, a CA slow to hand out a nonce. acme.sh's own retry
        # budget is not reachable from here, hence one more attempt.
        if [[ ${__try} -eq 1 ]] && grep -qE 'Cannot init API|Could not get nonce|libcurl-errors|curl error|: Timeout|timed out' "${__log}"; then
            echo -e "${yellow}The CA did not answer — retrying once...${plain}"
            continue
        fi
        break
    done
    rm -f "${__log}"

    if [[ ${__ok} -eq 1 ]]; then
        # acme.sh exits non-zero when reloadcmd fails, so check the files, not $?.
        ~/.acme.sh/acme.sh --installcert -d "$1" \
            --key-file "${__dir}/privkey.pem" \
            --fullchain-file "${__dir}/fullchain.pem" \
            --reloadcmd "${__reload}" >/dev/null 2>&1 || true
        if [[ ! -s "${__dir}/fullchain.pem" || ! -s "${__dir}/privkey.pem" ]]; then
            echo -e "${red}The certificate was issued but not installed into ${__dir}.${plain}"
            __ok=0
        fi
    fi
    if [[ ${__ok} -ne 1 ]]; then
        for __d in "${__created[@]}"; do
            rm -rf "${__d}"
        done
        [[ ${__dir_existed} -eq 1 ]] || rm -rf "${__dir}"
        return 1
    fi
    chmod 600 "${__dir}/privkey.pem" 2>/dev/null
    chmod 644 "${__dir}/fullchain.pem" 2>/dev/null
    # Keeps acme.sh current and its cron job in place: the renewals are the point.
    ~/.acme.sh/acme.sh --upgrade --auto-upgrade >/dev/null 2>&1
    return 0
}

# acme_migrate_to_webroot switches the certificates acme.sh renews in
# standalone mode over to nginx's webroot, without asking the CA for anything.
#
# A box installed before port 80 went to nginx has them that way, and with
# nginx on port 80 their next renewal would fail. The domain config acme.sh
# sources on --renew is edited in place: Le_Webroot — the one key renew reads
# to choose the challenge mode, "no" meaning standalone — becomes the webroot
# (Le_HTTPPort and Le_Listen_V4/V6 only matter to standalone and stay behind,
# harmless), and Le_ReloadCmd gains an nginx reload in front of whatever it
# ran before. A config on another webroot, on DNS or on ALPN is not ours and is
# left alone; a second run finds nothing to do.
acme_migrate_to_webroot() {
    local __home="${HOME:-/root}/.acme.sh" __dir __name __conf __roots __cmd __b64
    [[ -d "${__home}" ]] || return 0
    command -v openssl >/dev/null 2>&1 || return 0
    for __dir in "${__home}"/*/; do
        __dir="${__dir%/}"
        __name="$(basename "${__dir}")"
        __name="${__name%_ecc}"
        __conf="${__dir}/${__name}.conf"
        { [[ -f "${__conf}" ]] && grep -q "^Le_Domain=" "${__conf}"; } || continue
        __roots=$(sed -n "s/^Le_Webroot='\(.*\)'$/\1/p" "${__conf}" | head -n 1)
        # Standalone is "no" for every name the certificate carries.
        [[ -n "${__roots}" ]] || continue
        echo "${__roots}" | tr ',' '\n' | grep -qvx 'no' && continue

        # acme.sh keeps the reload command base64-encoded between markers;
        # an older acme.sh kept it plain.
        __cmd=$(sed -n "s/^Le_ReloadCmd='__ACME_BASE64__START_\(.*\)__ACME_BASE64__END_'$/\1/p" "${__conf}" | head -n 1)
        if [[ -n "${__cmd}" ]]; then
            __cmd=$(printf '%s' "${__cmd}" | openssl base64 -d -A 2>/dev/null)
        else
            __cmd=$(sed -n "s/^Le_ReloadCmd='\(.*\)'$/\1/p" "${__conf}" | head -n 1)
        fi
        if [[ -z "${__cmd}" ]]; then
            __cmd="$(acme_nginx_reload_cmd)"
        elif [[ "${__cmd}" != *"nginx -s reload"* ]]; then
            __cmd="$(acme_nginx_reload_cmd); ${__cmd}"
        fi
        __b64=$(printf '%s' "${__cmd}" | openssl base64 -e | tr -d '\r\n')

        sed -i -e "s|^Le_Webroot=.*$|Le_Webroot='$(acme_webroot)'|" -e "/^Le_ReloadCmd=/d" "${__conf}"
        [[ -n "$(tail -c 1 "${__conf}")" ]] && echo >>"${__conf}"
        echo "Le_ReloadCmd='__ACME_BASE64__START_${__b64}__ACME_BASE64__END_'" >>"${__conf}"
        echo -e "${green}acme.sh now renews ${__name} through nginx on port 80 instead of standalone.${plain}"
    done
}

# acme_front_setup runs at install and update time, on the panel and on the
# hops: nginx on port 80 in front of the ACME webroot, then acme.sh's
# standalone certificates switched over to it. Never fatal — a box where nginx
# cannot take port 80 keeps renewing the way it did, and says why; the rest of
# the run knows (acme_front_failed) and does not try again.
acme_front_setup() {
    if ! command -v nginx >/dev/null 2>&1; then
        echo -e "${yellow}nginx is not installed: certificates keep renewing the way they did.${plain}"
        acme_front_failed=1
        return 0
    fi
    if ! "${xui_folder}/x-ui" nginx acme-front; then
        echo -e "${yellow}nginx could not take port 80 for the ACME challenge (see above); certificates keep renewing the way they did.${plain}"
        acme_front_failed=1
        # A hop gets nginx with this release. One that came with this very run
        # and cannot serve port 80 must not stay there — nor come back at the
        # next boot — with the distro's default site, in the way of the
        # standalone renewals it was meant to replace.
        if [[ "${nginx_installed_now:-0}" == "1" && "${XUI_PROXY_MODE:-}" == "1" ]]; then
            systemctl disable --now nginx >/dev/null 2>&1 ||
                { rc-service nginx stop >/dev/null 2>&1; rc-update del nginx default >/dev/null 2>&1; } || true
        fi
        return 0
    fi
    acme_migrate_to_webroot
}

# hop_install_nginx brings nginx to a hop, where until now acme.sh's standalone
# listener had port 80 to itself. Two things are different from the panel's
# install_nginx:
#
#   - something other than nginx on port 80 (a renewal in progress, a service
#     of the operator's) keeps the port: nginx is not installed, and the hop
#     stays on the standalone path it has;
#   - the package must not start nginx. Debian's postinst would, with the
#     distro's default site on :80, before acme-front has had its say; a
#     policy-rc.d that answers 101 holds every service start back while the
#     package installs, and is removed again whatever happens. acme-front is
#     what first brings port 80 up, with the right config.
hop_install_nginx() {
    command -v nginx >/dev/null 2>&1 && return 0
    if is_port_in_use 80; then
        echo -e "${yellow}Something other than nginx holds port 80 — not installing nginx; certificates keep renewing the way they did.${plain}"
        return 0
    fi
    local __policy="${XUI_POLICY_RC_D:-/usr/sbin/policy-rc.d}" __own_policy=0
    if [[ ! -e "${__policy}" ]]; then
        printf '#!/bin/sh\nexit 101\n' >"${__policy}" && chmod 755 "${__policy}" && __own_policy=1
        # shellcheck disable=SC2064 # the path is fixed now, on purpose
        trap "rm -f '${__policy}'" EXIT INT TERM
    fi
    echo -e "${green}Installing nginx (not started: acme-front brings port 80 up)...${plain}"
    case "${release}" in
    ubuntu | debian | armbian)
        apt-get install -y -q nginx libnginx-mod-stream 2>/dev/null ||
            apt-get install -y -q nginx 2>/dev/null || true
        ;;
    fedora | amzn | rhel | almalinux | rocky | ol | centos)
        dnf install -y nginx nginx-mod-stream 2>/dev/null ||
            dnf install -y nginx 2>/dev/null ||
            yum install -y nginx 2>/dev/null || true
        ;;
    arch | manjaro | parch)
        pacman -Syu --noconfirm nginx 2>/dev/null || true
        ;;
    alpine)
        apk add nginx nginx-mod-stream 2>/dev/null || apk add nginx 2>/dev/null || true
        ;;
    *)
        echo -e "${yellow}Unknown OS — install nginx by hand; until then certificates keep renewing the way they did.${plain}"
        ;;
    esac
    if [[ ${__own_policy} -eq 1 ]]; then
        rm -f "${__policy}"
        trap - EXIT INT TERM
    fi
    # The package manager is a stub in the tests and prints nothing to rely
    # on; whether it ran is what matters to acme_front_setup.
    nginx_installed_now=1
}

# Issue Let's Encrypt IP certificate with shortlived profile (~6 days validity)
# through nginx's webroot on port 80 (acme_issue_webroot).
setup_ip_certificate() {
    local ipv4="$1"
    local ipv6="$2"  # optional

    echo -e "${green}Setting up Let's Encrypt IP certificate (shortlived profile)...${plain}"
    echo -e "${yellow}Note: IP certificates are valid for ~6 days and will auto-renew.${plain}"
    echo -e "${yellow}Port 80 must be reachable from the internet: nginx answers the CA there.${plain}"

    # Check for acme.sh
    if ! command -v ~/.acme.sh/acme.sh &>/dev/null; then
        install_acme
        if [ $? -ne 0 ]; then
            echo -e "${red}Failed to install acme.sh${plain}"
            return 1
        fi
    fi

    # Validate IP address
    if [[ -z "$ipv4" ]]; then
        echo -e "${red}IPv4 address is required${plain}"
        return 1
    fi

    if ! is_ipv4 "$ipv4"; then
        echo -e "${red}Invalid IPv4 address: $ipv4${plain}"
        return 1
    fi

    local certDir="/root/cert/ip"
    local -a names=("${ipv4}")
    if [[ -n "$ipv6" ]] && is_ipv6 "$ipv6"; then
        names+=("${ipv6}")
        echo -e "${green}Including IPv6 address: ${ipv6}${plain}"
    fi

    # acme_issue_webroot cleans up after a failure itself, and only what the
    # attempt created: a certificate that works stays.
    echo -e "${green}Issuing IP certificate for ${ipv4}...${plain}"
    if ! acme_issue_webroot "${certDir}" "" "${names[@]}"; then
        echo -e "${red}Failed to issue IP certificate${plain}"
        echo -e "${yellow}Please ensure port 80 is reachable from the internet${plain}"
        return 1
    fi
    echo -e "${green}Certificate files installed successfully${plain}"

    # Configure panel to use the certificate
    echo -e "${green}Setting certificate paths for the panel...${plain}"
    ${xui_folder}/x-ui cert -webCert "${certDir}/fullchain.pem" -webCertKey "${certDir}/privkey.pem"

    if [ $? -ne 0 ]; then
        echo -e "${yellow}Warning: Could not set certificate paths automatically${plain}"
        echo -e "${yellow}Certificate files are at:${plain}"
        echo -e "  Cert: ${certDir}/fullchain.pem"
        echo -e "  Key:  ${certDir}/privkey.pem"
    else
        echo -e "${green}Certificate paths configured successfully${plain}"
    fi

    echo -e "${green}IP certificate installed and configured successfully!${plain}"
    echo -e "${green}Certificate valid for ~6 days, auto-renews via acme.sh cron job.${plain}"
    echo -e "${yellow}After each renewal acme.sh reloads nginx and restarts x-ui.${plain}"
    return 0
}

# Comprehensive manual SSL certificate issuance via acme.sh
ssl_cert_issue() {
    local existing_webBasePath=$(${xui_folder}/x-ui setting -show true | grep 'webBasePath:' | awk -F': ' '{print $2}' | tr -d '[:space:]' | sed 's#^/##')
    local existing_port=$(${xui_folder}/x-ui setting -show true | grep 'port:' | awk -F': ' '{print $2}' | tr -d '[:space:]')
    
    # check for acme.sh first
    if ! command -v ~/.acme.sh/acme.sh &>/dev/null; then
        echo "acme.sh could not be found. Installing now..."
        cd ~ || return 1
        curl -s https://get.acme.sh | sh
        if [ $? -ne 0 ]; then
            echo -e "${red}Failed to install acme.sh${plain}"
            return 1
        else
            echo -e "${green}acme.sh installed successfully${plain}"
        fi
    fi

    # get the domain here, and we need to verify it
    local domain=""
    while true; do
        read -rp "Please enter your domain name: " domain
        domain="${domain// /}"  # Trim whitespace
        
        if [[ -z "$domain" ]]; then
            echo -e "${red}Domain name cannot be empty. Please try again.${plain}"
            continue
        fi
        
        if ! is_domain "$domain"; then
            echo -e "${red}Invalid domain format: ${domain}. Please enter a valid domain name.${plain}"
            continue
        fi
        
        break
    done
    echo -e "${green}Your domain is: ${domain}, checking it...${plain}"

    # remember the domain for the caller (Access URL)
    ISSUED_DOMAIN="${domain}"

    # check if there already exists a certificate
    if acme_cert_domains | grep -Fxq "${domain}"; then
        echo -e "${yellow}acme.sh already has a certificate for ${domain}:${plain}"
        ~/.acme.sh/acme.sh --list
        read -rp "Re-issue it now (the existing certificate will be overwritten)? (y/n): " reissue
        if [[ "$reissue" != "y" && "$reissue" != "Y" ]]; then
            echo -e "${green}Keeping the existing certificate.${plain}"
            return 0
        fi
    else
        echo -e "${green}Your domain is ready for issuing certificates now...${plain}"
    fi

    # The directory for the certificate. An existing one is kept: the new
    # files replace the old ones only once they have been issued.
    certPath="/root/cert/${domain}"

    # The reload command runs on every issue and renewal. The default reloads
    # nginx — it serves this certificate on 443 once the front is on — and
    # restarts x-ui, which serves it on the panel port.
    local reloadCmd=""
    echo -e "${green}Default --reloadcmd for ACME: ${yellow}$(acme_reload_cmd)${plain}"
    echo -e "${green}This command will run on every certificate issue and renew.${plain}"
    read -rp "Would you like to use your own --reloadcmd instead? (y/n): " setReloadcmd
    if [[ "$setReloadcmd" == "y" || "$setReloadcmd" == "Y" ]]; then
        echo -e "${yellow}Keep an nginx reload in it, and put x-ui restart at the end.${plain}"
        read -rp "Please enter your custom reloadcmd: " reloadCmd
        echo -e "${green}Reloadcmd is: ${reloadCmd}${plain}"
    fi

    # nginx answers the challenge on port 80 from its webroot, so the panel
    # keeps running and nothing has to give port 80 up.
    echo -e "${yellow}Port 80 must be reachable from the internet: nginx answers the CA there.${plain}"
    if ! acme_issue_webroot "${certPath}" "${reloadCmd}" "${domain}"; then
        echo -e "${red}Issuing certificate failed, please check logs.${plain}"
        ISSUED_DOMAIN=""
        return 1
    fi
    echo -e "${green}Certificate issued and installed; acme.sh renews it from cron:${plain}"
    ls -lah ${certPath}/

    # Prompt user to set panel paths after successful certificate installation
    read -rp "Would you like to set this certificate for the panel? (Y/n): " setPanel
    # Empty answer means yes: a certificate that was just issued for this
    # panel is almost always meant to be used by it, and skipping the step
    # silently leaves the panel serving the previous certificate.
    if [[ -z "$setPanel" || "$setPanel" == "y" || "$setPanel" == "Y" ]]; then
        local webCertFile="/root/cert/${domain}/fullchain.pem"
        local webKeyFile="/root/cert/${domain}/privkey.pem"

        if [[ -f "$webCertFile" && -f "$webKeyFile" ]]; then
            ${xui_folder}/x-ui cert -webCert "$webCertFile" -webCertKey "$webKeyFile"
            echo -e "${green}Certificate paths set for the panel${plain}"
            echo -e "${green}Certificate File: $webCertFile${plain}"
            echo -e "${green}Private Key File: $webKeyFile${plain}"
            echo ""
            echo -e "${green}Access URL: https://${domain}:${existing_port}/${existing_webBasePath}${plain}"
            echo -e "${yellow}Panel will restart to apply SSL certificate...${plain}"
            systemctl restart x-ui 2>/dev/null || rc-service x-ui restart 2>/dev/null
        else
            echo -e "${red}Error: Certificate or private key file not found for domain: $domain.${plain}"
        fi
    else
        echo -e "${yellow}Skipping panel path setting.${plain}"
    fi
    
    return 0
}
# Unified interactive SSL setup (domain or IP)
# Sets global `SSL_HOST` to the chosen domain/IP
prompt_and_setup_ssl() {
    local panel_port="$1"
    local web_base_path="$2"   # expected without leading slash
    local server_ip="$3"

    local ssl_choice=""

    echo -e "${yellow}Choose SSL certificate setup method:${plain}"
    echo -e "${green}1.${plain} Let's Encrypt for Domain (90-day validity, auto-renews)"
    echo -e "${green}2.${plain} Let's Encrypt for IP Address (6-day validity, auto-renews)"
    echo -e "${green}3.${plain} Custom SSL Certificate (Path to existing files)"
    echo -e "${blue}Note:${plain} Options 1 & 2 require port 80 open. Option 3 requires manual paths."
    read -rp "Choose an option (default 2 for IP): " ssl_choice
    ssl_choice="${ssl_choice// /}"  # Trim whitespace
    
    # Default to 2 (IP cert) if input is empty or invalid (not 1 or 3)
    if [[ "$ssl_choice" != "1" && "$ssl_choice" != "3" ]]; then
        ssl_choice="2"
    fi

    case "$ssl_choice" in
    1)
        # User chose Let's Encrypt domain option
        echo -e "${green}Using Let's Encrypt for domain certificate...${plain}"
        ISSUED_DOMAIN=""
        ssl_cert_issue
        # ssl_cert_issue reports the domain it worked on via ISSUED_DOMAIN
        local cert_domain="${ISSUED_DOMAIN}"
        if [[ -n "${cert_domain}" ]]; then
            SSL_HOST="${cert_domain}"
            echo -e "${green}✓ SSL certificate configured successfully with domain: ${cert_domain}${plain}"
        else
            echo -e "${yellow}No certificate was issued; using the IP address for the Access URL${plain}"
            SSL_HOST="${server_ip}"
        fi
        ;;
    2)
        # User chose Let's Encrypt IP certificate option
        echo -e "${green}Using Let's Encrypt for IP certificate (shortlived profile)...${plain}"
        
        # Ask for optional IPv6
        local ipv6_addr=""
        read -rp "Do you have an IPv6 address to include? (leave empty to skip): " ipv6_addr
        ipv6_addr="${ipv6_addr// /}"  # Trim whitespace
        
        # Stop the panel so that it comes back up with the certificate set
        # below (port 80 is nginx's now; the panel never needs to free it).
        if [[ $release == "alpine" ]]; then
            rc-service x-ui stop >/dev/null 2>&1
        else
            systemctl stop x-ui >/dev/null 2>&1
        fi
        
        setup_ip_certificate "${server_ip}" "${ipv6_addr}"
        if [ $? -eq 0 ]; then
            SSL_HOST="${server_ip}"
            echo -e "${green}✓ Let's Encrypt IP certificate configured successfully${plain}"
        else
            echo -e "${red}✗ IP certificate setup failed. Please check port 80 is open.${plain}"
            SSL_HOST="${server_ip}"
        fi
        
        # Restart panel after SSL is configured (restart applies new cert settings)
        if [[ $release == "alpine" ]]; then
            rc-service x-ui restart >/dev/null 2>&1
        else
            systemctl restart x-ui >/dev/null 2>&1
        fi

        ;;
    3)
        # User chose Custom Paths (User Provided) option
        echo -e "${green}Using custom existing certificate...${plain}"
        local custom_cert=""
        local custom_key=""
        local custom_domain=""

        # 3.1 Request Domain to compose Panel URL later
        read -rp "Please enter domain name certificate issued for: " custom_domain
        custom_domain="${custom_domain// /}" # Убираем пробелы

        # 3.2 Loop for Certificate Path
        while true; do
            read -rp "Input certificate path (keywords: .crt / fullchain): " custom_cert
            # Strip quotes if present
            custom_cert=$(echo "$custom_cert" | tr -d '"' | tr -d "'")

            if [[ -f "$custom_cert" && -r "$custom_cert" && -s "$custom_cert" ]]; then
                break
            elif [[ ! -f "$custom_cert" ]]; then
                echo -e "${red}Error: File does not exist! Try again.${plain}"
            elif [[ ! -r "$custom_cert" ]]; then
                echo -e "${red}Error: File exists but is not readable (check permissions)!${plain}"
            else
                echo -e "${red}Error: File is empty!${plain}"
            fi
        done

        # 3.3 Loop for Private Key Path
        while true; do
            read -rp "Input private key path (keywords: .key / privatekey): " custom_key
            # Strip quotes if present
            custom_key=$(echo "$custom_key" | tr -d '"' | tr -d "'")

            if [[ -f "$custom_key" && -r "$custom_key" && -s "$custom_key" ]]; then
                break
            elif [[ ! -f "$custom_key" ]]; then
                echo -e "${red}Error: File does not exist! Try again.${plain}"
            elif [[ ! -r "$custom_key" ]]; then
                echo -e "${red}Error: File exists but is not readable (check permissions)!${plain}"
            else
                echo -e "${red}Error: File is empty!${plain}"
            fi
        done

        # 3.4 Apply Settings via x-ui binary
        ${xui_folder}/x-ui cert -webCert "$custom_cert" -webCertKey "$custom_key" >/dev/null 2>&1

        # Set SSL_HOST for composing Panel URL
        if [[ -n "$custom_domain" ]]; then
            SSL_HOST="$custom_domain"
        else
            SSL_HOST="${server_ip}"
        fi

        echo -e "${green}✓ Custom certificate paths applied.${plain}"
        echo -e "${yellow}Note: You are responsible for renewing these files externally.${plain}"

        systemctl restart x-ui >/dev/null 2>&1 || rc-service x-ui restart >/dev/null 2>&1
        ;;
    *)
        echo -e "${red}Invalid option. Skipping SSL setup.${plain}"
        SSL_HOST="${server_ip}"
        ;;
    esac
}

# Localhost-only debug update. Plain HTTP, listen=127.0.0.1, port default
# 8080 (kept if already set), no SSL prompt, no public-IP detection.
config_debug_mode_after_update() {
    echo -e "${yellow}x-ui settings (debug mode):${plain}"
    ${xui_folder}/x-ui setting -show true
    ${xui_folder}/x-ui migrate

    local existing_port=$(${xui_folder}/x-ui setting -show true | grep -Eo 'port: .+' | awk '{print $2}')
    local existing_webBasePath=$(${xui_folder}/x-ui setting -show true | grep -Eo 'webBasePath: .+' | awk '{print $2}' | sed 's#^/##')

    # Prefer the port the user picked at the start of the update; only
    # fall back to whatever was previously configured if they didn't
    # answer the prompt (e.g. running with an older XUI_DEBUG_PORT env).
    local desired_port="${XUI_DEBUG_PORT:-${existing_port}}"
    if [[ -z "${desired_port}" || "${desired_port}" == "0" ]]; then
        desired_port=8080
    fi
    if [[ "${desired_port}" != "${existing_port}" ]]; then
        ${xui_folder}/x-ui setting -port "${desired_port}"
        existing_port="${desired_port}"
    fi
    if [[ ${#existing_webBasePath} -lt 4 ]]; then
        existing_webBasePath=$(gen_random_string 18)
        ${xui_folder}/x-ui setting -webBasePath "${existing_webBasePath}"
    fi

    # Force loopback bind on every update so a previously-public install
    # can be safely flipped to debug mode.
    ${xui_folder}/x-ui setting -listenIP "127.0.0.1"

    echo ""
    echo -e "${green}═══════════════════════════════════════════${plain}"
    echo -e "${green}  Panel updated in DEBUG / localhost mode    ${plain}"
    echo -e "${green}═══════════════════════════════════════════${plain}"
    echo -e "${green}Port:        ${existing_port}${plain}"
    echo -e "${green}WebBasePath: ${existing_webBasePath}${plain}"
    echo -e "${green}Listen:      127.0.0.1 (loopback only)${plain}"
    echo -e "${green}Access URL:  http://127.0.0.1:${existing_port}/${existing_webBasePath}${plain}"
    echo -e "${green}             http://localhost:${existing_port}/${existing_webBasePath}${plain}"
    echo -e "${green}═══════════════════════════════════════════${plain}"
}

config_after_update() {
    if [[ "${XUI_PROXY_MODE:-}" == "1" ]]; then
        echo -e "${green}Proxy-front mode — keeping /etc/x-ui/proxy.json, /etc/x-ui/chain/ and the certificate unchanged.${plain}"
        # The certificate stays; how acme.sh renews it changes: through nginx
        # on port 80 rather than a listener of its own.
        acme_front_setup
        # Legacy configs never reach this point: proxy_config_gate stops the
        # update before the binary is replaced (§5.7). What is left to say is
        # the one thing a v2 box may still be missing.
        if [[ "${proxy_not_joined:-0}" == "1" ]]; then
            echo -e "${yellow}This box has not joined the chain yet — the join page is at: x-ui chain join-url${plain}"
        fi
        return
    fi
    if [[ "${XUI_DEBUG_MODE:-}" == "1" ]]; then
        config_debug_mode_after_update
        return
    fi

    echo -e "${yellow}x-ui settings:${plain}"
    ${xui_folder}/x-ui setting -show true
    ${xui_folder}/x-ui migrate

    # Port 80 goes to nginx, and the certificates acme.sh renewed standalone
    # renew through its webroot from now on (before any issuance below).
    acme_front_setup
    
    # Properly detect empty cert by checking if cert: line exists and has content after it
    local existing_cert=$(${xui_folder}/x-ui setting -getCert true 2>/dev/null | grep 'cert:' | awk -F': ' '{print $2}' | tr -d '[:space:]')
    local existing_port=$(${xui_folder}/x-ui setting -show true | grep -Eo 'port: .+' | awk '{print $2}')
    local existing_webBasePath=$(${xui_folder}/x-ui setting -show true | grep -Eo 'webBasePath: .+' | awk '{print $2}' | sed 's#^/##')
    
    # Get server IP
    local URL_lists=(
        "https://api4.ipify.org"
        "https://ipv4.icanhazip.com"
        "https://v4.api.ipinfo.io/ip"
        "https://ipv4.myexternalip.com/raw"
        "https://4.ident.me"
        "https://check-host.net/ip"
    )
    local server_ip=""
    for ip_address in "${URL_lists[@]}"; do
        local response=$(curl -s -w "\n%{http_code}" --max-time 3 "${ip_address}" 2>/dev/null)
        local http_code=$(echo "$response" | tail -n1)
        local ip_result=$(echo "$response" | head -n-1 | tr -d '[:space:]')
        if [[ "${http_code}" == "200" && -n "${ip_result}" ]]; then
            server_ip="${ip_result}"
            break
        fi
    done
    
    # Handle missing/short webBasePath
    if [[ ${#existing_webBasePath} -lt 4 ]]; then
        echo -e "${yellow}WebBasePath is missing or too short. Generating a new one...${plain}"
        local config_webBasePath=$(gen_random_string 18)
        ${xui_folder}/x-ui setting -webBasePath "${config_webBasePath}"
        existing_webBasePath="${config_webBasePath}"
        echo -e "${green}New WebBasePath: ${config_webBasePath}${plain}"
    fi
    
    # Check and prompt for SSL if missing
    if [[ -z "$existing_cert" ]]; then
        echo ""
        echo -e "${red}═══════════════════════════════════════════${plain}"
        echo -e "${red}      ⚠ NO SSL CERTIFICATE DETECTED ⚠     ${plain}"
        echo -e "${red}═══════════════════════════════════════════${plain}"
        echo -e "${yellow}For security, SSL certificate is MANDATORY for all panels.${plain}"
        echo -e "${yellow}Let's Encrypt now supports both domains and IP addresses!${plain}"
        echo ""
        
        if [[ -z "${server_ip}" ]]; then
            echo -e "${red}Failed to detect server IP${plain}"
            echo -e "${yellow}Please configure SSL manually using: x-ui${plain}"
            return
        fi
        
        # Prompt and setup SSL (domain or IP)
        prompt_and_setup_ssl "${existing_port}" "${existing_webBasePath}" "${server_ip}"
        
        echo ""
        echo -e "${green}═══════════════════════════════════════════${plain}"
        echo -e "${green}     Panel Access Information              ${plain}"
        echo -e "${green}═══════════════════════════════════════════${plain}"
        echo -e "${green}Access URL: https://${SSL_HOST}:${existing_port}/${existing_webBasePath}${plain}"
        echo -e "${green}═══════════════════════════════════════════${plain}"
        echo -e "${yellow}⚠ SSL Certificate: Enabled and configured${plain}"
    else
        echo -e "${green}SSL certificate is already configured${plain}"
        # Show access URL with existing certificate. IP certificates are stored
        # in /root/cert/ip, so the directory name is the literal "ip"; a
        # self-signed certificate (install.sh's generate_self_signed_cert)
        # lives in /root/cert/self-signed regardless of which host it was
        # actually issued for. Both directory names are placeholders, not
        # hosts — printing them verbatim gave a live box's footer
        # "Access URL: https://self-signed:PORT/...". install.sh's own
        # footer never has this problem because it keeps SSL_HOST from the
        # box's resolved address instead of round-tripping it through the
        # cert path; do the same here.
        local cert_domain=$(basename "$(dirname "$existing_cert")")
        local access_host="$cert_domain"
        if [[ "$cert_domain" == "ip" || "$cert_domain" == "self-signed" ]]; then
            access_host="${server_ip:-$cert_domain}"
        fi
        echo ""
        echo -e "${green}═══════════════════════════════════════════${plain}"
        echo -e "${green}     Panel Access Information              ${plain}"
        echo -e "${green}═══════════════════════════════════════════${plain}"
        echo -e "${green}Access URL: https://${access_host}:${existing_port}/${existing_webBasePath}${plain}"
        echo -e "${green}═══════════════════════════════════════════${plain}"
    fi
}

# Translates the panel's arch label to xray-core's release naming so we can
# fetch the right Xray-linux-{ARCH}.zip from XTLS/Xray-core releases.
# install_nginx puts nginx on the server so the panel can consolidate every TLS
# protocol onto port 443. It is installed unconditionally, before the panel, for
# the same reason AmneziaWG is: the panel decides on first start what it can
# offer, and a front-end that appears only after the next update is a front-end
# nobody finds.
#
# Nothing here fails the installation. A server without nginx keeps working
# exactly as before — it simply cannot hide its protocols behind one port.
#
# Port 80 belongs to nginx on every box, where it answers the ACME challenge
# (acme_front_setup, acme_issue_webroot). Hops get nginx from hop_install_nginx
# instead, which leaves it stopped for acme-front to bring up.
install_nginx() {
    if command -v nginx &>/dev/null; then
        echo -e "${green}nginx already installed: $(nginx -v 2>&1 | sed 's|.*nginx/||')${plain}"
    else
        echo -e "${green}Installing nginx...${plain}"
        case "${release}" in
            ubuntu | debian | armbian)
                # On Debian and its derivatives the stream module is a separate
                # package, and without it nginx cannot split 443 by SNI at all.
                apt-get install -y -q nginx libnginx-mod-stream 2>/dev/null ||
                    apt-get install -y -q nginx 2>/dev/null || true
                ;;
            fedora | amzn | rhel | almalinux | rocky | ol | centos)
                dnf install -y nginx nginx-mod-stream 2>/dev/null ||
                    dnf install -y nginx 2>/dev/null ||
                    yum install -y nginx 2>/dev/null || true
                ;;
            arch | manjaro | parch)
                pacman -Syu --noconfirm nginx 2>/dev/null || true
                ;;
            alpine)
                apk add nginx nginx-mod-stream 2>/dev/null || apk add nginx 2>/dev/null || true
                ;;
            *)
                echo -e "${yellow}Unknown OS — install nginx by hand to use the «everything on 443» modes.${plain}"
                ;;
        esac
    fi

    if ! command -v nginx &>/dev/null; then
        echo -e "${yellow}nginx was not installed. The panel works as before; the Nginx page will${plain}"
        echo -e "${yellow}stay unavailable until nginx is installed.${plain}"
        return
    fi

    # The panel owns this directory: a distro's nginx.conf includes conf.d from
    # inside http {}, where a stream block is a syntax error, so the stream part
    # of the config needs a home of its own.
    mkdir -p /etc/nginx/stream-enabled /usr/local/x-ui/www

    if ! nginx -V 2>&1 | grep -q -- '--with-stream' &&
        ! ls /etc/nginx/modules-enabled/*stream*.conf &>/dev/null; then
        echo -e "${yellow}This nginx has no stream module, so port 443 cannot be split by server name.${plain}"
        case "${release}" in
            ubuntu | debian | armbian) echo -e "${yellow}  Fix: apt-get install -y libnginx-mod-stream${plain}" ;;
            alpine) echo -e "${yellow}  Fix: apk add nginx-mod-stream${plain}" ;;
            *) echo -e "${yellow}  Install the nginx stream module for your distribution.${plain}" ;;
        esac
    fi

    # A configuration the panel generated earlier may not be accepted by a newer
    # nginx. Saying so here is the difference between a five-minute fix and a
    # server where 443 is quietly down after an update.
    if ! nginx -t &>/dev/null; then
        echo -e "${red}nginx refuses the current configuration:${plain}"
        nginx -t 2>&1 | sed 's/^/    /'
        echo -e "${yellow}Port 443 stays down until this is fixed. The panel rewrites its own part${plain}"
        echo -e "${yellow}of the config from the Nginx page — reapplying the mode there is usually enough.${plain}"
        return
    fi

    # A server that reboots without nginx comes back with 443 shut and every
    # protocol behind it unreachable.
    if command -v systemctl &>/dev/null; then
        systemctl enable nginx &>/dev/null || true
        systemctl start nginx &>/dev/null || systemctl reload nginx &>/dev/null || true
    elif command -v rc-update &>/dev/null; then
        rc-update add nginx default &>/dev/null || true
        rc-service nginx start &>/dev/null || true
    fi

    echo -e "${green}nginx: $(nginx -v 2>&1 | sed 's|.*nginx/||')${plain}"
}

xray_release_arch() {
    case "$(arch)" in
        amd64) echo "64" ;;
        386) echo "32" ;;
        arm64) echo "arm64-v8a" ;;
        armv7) echo "arm32-v7a" ;;
        armv6) echo "arm32-v6" ;;
        armv5) echo "arm32-v5" ;;
        s390x) echo "s390x" ;;
        *) echo "" ;;
    esac
}

# Translates the panel's arch label to the filename the panel uses for the
# bundled xray binary (panel looks up bin/xray-linux-{FNAME}).
xray_panel_arch() {
    case "$(arch)" in
        amd64) echo "amd64" ;;
        386) echo "386" ;;
        arm64) echo "arm64" ;;
        armv7|armv6|armv5) echo "arm" ;;
        s390x) echo "s390x" ;;
        *) echo "" ;;
    esac
}

# Pinned mtg (MTProto FakeTLS sidecar, github.com/9seconds/mtg) version.
MTG_VER="2.2.8"

# Pinned xray-core (github.com/XTLS/Xray-core) version. One source of truth:
# it builds the download URL and decides whether the copy already on disk is
# worth replacing.
XRAY_VER="26.3.27"

# installed_xray_version prints the version of the xray binary at $1, or
# nothing when there is no binary to ask.
installed_xray_version() {
    local bin="$1"
    [[ -x "$bin" ]] || return 1
    "$bin" -version 2>/dev/null | head -n1 | awk '{print $2}'
}

# xray_is_current reports whether the binary at $1 is already the pinned
# release or newer. Newer counts: an operator who upgraded xray by hand is not
# asking for it to be put back.
xray_is_current() {
    local have
    have=$(installed_xray_version "$1") || return 1
    [[ -n "$have" ]] || return 1
    [[ "$(printf '%s\n%s\n' "$XRAY_VER" "$have" | sort -V | head -n1)" == "$XRAY_VER" ]]
}

# fetch_geo_file refreshes one geo-data file, and only when the server has
# something newer. The six of them come to about 145 MB, which used to be
# downloaded again on every single update.
#
# `-z` sends If-Modified-Since from the file's timestamp and `-R` sets that
# timestamp from the server's Last-Modified — the two are a pair, and without
# the second the first has nothing to ask about. A 304 leaves the file on disk
# untouched.
#
# The reachability prompt is only for a file we do not have. Once there is a
# usable copy, a server that cannot be reached is not worth stopping an update
# over.
fetch_geo_file() {
    local dest="$1" url="$2" label="$3" code
    if [[ ! -f "$dest" ]]; then
        check_url_or_skip "$url" "$label" || return 0
        if ${curl_bin:-curl} -4sfLRo "$dest" "$url"; then
            echo -e "  ${label}: downloaded"
        else
            echo -e "  ${yellow}${label}: could not be downloaded${plain}"
        fi
        return 0
    fi
    code=$(${curl_bin:-curl} -4sfLR -z "$dest" -o "$dest" -w '%{http_code}' "$url" 2>/dev/null)
    case "$code" in
        304) echo -e "  ${label}: up to date" ;;
        200) echo -e "  ${green}${label}: updated${plain}" ;;
        *)   echo -e "  ${yellow}${label}: kept the copy on disk (HTTP ${code:-none})${plain}" ;;
    esac
}

# mtg release-asset arch (empty = no prebuilt binary; s390x has none, armv5
# falls back to the armv6 build).
mtg_release_arch() {
    case "$(arch)" in
        amd64) echo "amd64" ;;
        386) echo "386" ;;
        arm64) echo "arm64" ;;
        armv7) echo "armv7" ;;
        armv6|armv5) echo "armv6" ;;
        *) echo "" ;;
    esac
}

# On-disk filename Go's mtproto package looks up: bin/mtg-linux-{FNAME}
# (FNAME == runtime.GOARCH, so all 32-bit arm collapse to "arm").
mtg_panel_arch() {
    case "$(arch)" in
        amd64) echo "amd64" ;;
        386) echo "386" ;;
        arm64) echo "arm64" ;;
        armv7|armv6|armv5) echo "arm" ;;
        *) echo "" ;;
    esac
}

# Ensures the mtg sidecar is present in the given bin dir as mtg-linux-{FNAME}.
# No-op when already present (preserved across updates). Fully non-fatal: any
# failure prints a notice and returns 0 — MTProto inbounds just won't start.
# mtg-multi (dolonet/mtg-multi) = multi-user MTProto fork; prebuilt only for
# linux amd64/arm64. Empty otherwise (fall back to single-secret mtg).
mtg_multi_arch() {
    case "$(arch)" in
        amd64) echo "amd64" ;;
        arm64) echo "arm64" ;;
        *) echo "" ;;
    esac
}

# Installs latest mtg-multi as bin/mtg-multi-linux-{FNAME}; returns 1 on failure.
install_mtg_multi() {
    local target_bin_dir="$1"
    local mm_arch mm_fname mm_ver mm_url tmp_tgz tmp_dir extracted installed_bin installed_ver
    mm_arch=$(mtg_multi_arch)
    mm_fname=$(mtg_panel_arch)
    [[ -z "$mm_arch" || -z "$mm_fname" ]] && return 1
    installed_bin="$target_bin_dir/mtg-multi-linux-${mm_fname}"

    # The user opted for "always latest": resolve the newest release tag and, if a
    # binary is already installed, upgrade it only when it is out of date.
    mm_ver=$(${curl_bin:-curl} -4 -Ls "https://api.github.com/repos/dolonet/mtg-multi/releases/latest" 2>/dev/null \
        | grep '"tag_name":' | sed -E 's/.*"v?([^"]+)".*/\1/' | head -n1)
    if [[ -f "$installed_bin" ]]; then
        chmod +x "$installed_bin" >/dev/null 2>&1
        # Can't resolve the latest version (e.g. API rate limit) — keep what we have.
        [[ -z "$mm_ver" ]] && return 0
        installed_ver=$("$installed_bin" --version 2>/dev/null | awk '{print $1}')
        if [[ "$installed_ver" == "$mm_ver" ]]; then
            return 0
        fi
        echo -e "${green}Updating mtg-multi ${installed_ver:-unknown} -> ${mm_ver}...${plain}"
    fi
    [[ -z "$mm_ver" ]] && return 1
    mm_url="https://github.com/dolonet/mtg-multi/releases/download/v${mm_ver}/mtg-multi-${mm_ver}-linux-${mm_arch}.tar.gz"
    echo -e "${green}Downloading mtg-multi (multi-user MTProto)...${plain}"
    tmp_tgz="/tmp/mtgmulti.$$.tar.gz"
    tmp_dir="/tmp/mtgmulti.$$.d"
    if ! ${curl_bin:-curl} -4fLRo "$tmp_tgz" "$mm_url" >/dev/null 2>&1; then
        rm -f "$tmp_tgz" >/dev/null 2>&1
        return 1
    fi
    mkdir -p "$tmp_dir" "$target_bin_dir" >/dev/null 2>&1
    if tar -xzf "$tmp_tgz" -C "$tmp_dir" >/dev/null 2>&1; then
        extracted=$(find "$tmp_dir" -type f -name mtg-multi 2>/dev/null | head -n1)
        if [[ -n "$extracted" ]]; then
            mv -f "$extracted" "$target_bin_dir/mtg-multi-linux-${mm_fname}" >/dev/null 2>&1
            chmod +x "$target_bin_dir/mtg-multi-linux-${mm_fname}" >/dev/null 2>&1
            rm -f "$target_bin_dir/mtg-linux-${mm_fname}" >/dev/null 2>&1
            echo -e "${green}mtg-multi installed (multi-user MTProto).${plain}"
            rm -rf "$tmp_tgz" "$tmp_dir" >/dev/null 2>&1
            return 0
        fi
    fi
    rm -rf "$tmp_tgz" "$tmp_dir" >/dev/null 2>&1
    return 1
}

install_mtg() {
    local target_bin_dir="$1"
    local mtg_arch mtg_fname mtg_url tmp_tgz tmp_dir extracted
    # Prefer the multi-user mtg-multi fork where a prebuilt binary exists.
    if install_mtg_multi "$target_bin_dir"; then
        return 0
    fi
    mtg_arch=$(mtg_release_arch)
    mtg_fname=$(mtg_panel_arch)
    if [[ -z "$mtg_arch" || -z "$mtg_fname" ]]; then
        return 0
    fi
    if [[ -f "$target_bin_dir/mtg-linux-${mtg_fname}" ]]; then
        chmod +x "$target_bin_dir/mtg-linux-${mtg_fname}" >/dev/null 2>&1
        return 0
    fi
    mtg_url="https://github.com/9seconds/mtg/releases/download/v${MTG_VER}/mtg-${MTG_VER}-linux-${mtg_arch}.tar.gz"
    echo -e "${green}Downloading mtg (MTProto sidecar)...${plain}"
    tmp_tgz="/tmp/mtg.$$.tar.gz"
    tmp_dir="/tmp/mtg.$$.d"
    if ! ${curl_bin:-curl} -4fLRo "$tmp_tgz" "$mtg_url" >/dev/null 2>&1; then
        rm -f "$tmp_tgz" >/dev/null 2>&1
        echo -e "${yellow}Could not download mtg — MTProto proxies will be unavailable.${plain}"
        return 0
    fi
    mkdir -p "$tmp_dir" "$target_bin_dir" >/dev/null 2>&1
    if tar -xzf "$tmp_tgz" -C "$tmp_dir" >/dev/null 2>&1; then
        extracted=$(find "$tmp_dir" -type f -name mtg 2>/dev/null | head -n1)
        if [[ -n "$extracted" ]]; then
            mv -f "$extracted" "$target_bin_dir/mtg-linux-${mtg_fname}" >/dev/null 2>&1
            chmod +x "$target_bin_dir/mtg-linux-${mtg_fname}" >/dev/null 2>&1
            echo -e "${green}mtg installed as bin/mtg-linux-${mtg_fname}.${plain}"
        fi
    fi
    rm -rf "$tmp_tgz" "$tmp_dir" >/dev/null 2>&1
    return 0
}

# Downloads xray binary + geo data files into the given target directory.
# Mirrors the logic in DockerInit.sh — same xray version (v26.3.27), same
# geo-data sources.
download_xray_and_geo() {
    local target_bin_dir="$1"
    local xray_arch xray_fname xray_url
    xray_arch=$(xray_release_arch)
    xray_fname=$(xray_panel_arch)
    if [[ -z "$xray_arch" || -z "$xray_fname" ]]; then
        echo -e "${red}No prebuilt xray-core for arch $(arch).${plain}"
        return 1
    fi
    if ! _command_exists unzip; then
        echo -e "${yellow}Installing unzip (needed to extract xray-core)...${plain}"
        case "${release}" in
            ubuntu|debian|armbian) apt-get install -y -q unzip >/dev/null 2>&1 ;;
            arch|manjaro|parch)    pacman -Sy --noconfirm unzip >/dev/null 2>&1 ;;
            alpine)                apk add unzip >/dev/null 2>&1 ;;
            opensuse-tumbleweed)   zypper install -y unzip >/dev/null 2>&1 ;;
            *)                     dnf install -y -q unzip >/dev/null 2>&1 || yum install -y unzip >/dev/null 2>&1 ;;
        esac
    fi

    mkdir -p "$target_bin_dir"
    local tmp_zip="/tmp/xray-core.$$.zip"
    local xray_bin="$target_bin_dir/xray-linux-${xray_fname}"
    xray_url="https://github.com/XTLS/Xray-core/releases/download/v${XRAY_VER}/Xray-linux-${xray_arch}.zip"

    # Twenty megabytes for a binary that is very often already there, and on
    # the local-source path it used to be thrown away straight afterwards by
    # the restore of the preserved copy.
    if xray_is_current "$xray_bin"; then
        echo -e "${green}xray-core $(installed_xray_version "$xray_bin") already installed, not downloading it again.${plain}"
    else
        # xray binary is mandatory — without it the panel can't run any
        # protocol. Probe the URL up-front so the user gets a clean prompt
        # instead of waiting through curl's full retry loop on a dead host.
        if ! check_url_or_skip "$xray_url" "xray-core binary"; then
            echo -e "${red}Cannot proceed without xray-core — aborting xray bundle download.${plain}"
            return 1
        fi
        echo -e "${green}Downloading xray-core ${xray_url}...${plain}"
        if ! ${curl_bin} -4fLRo "$tmp_zip" "$xray_url"; then
            rm -f "$tmp_zip"
            echo -e "${red}Failed to download xray-core.${plain}"
            return 1
        fi
        (cd "$target_bin_dir" && unzip -o "$tmp_zip" >/dev/null) || {
            rm -f "$tmp_zip"
            echo -e "${red}Failed to unzip xray-core.${plain}"
            return 1
        }
        rm -f "$tmp_zip"
        # The zip carries its own geo files, which are the ones we replace
        # below. Only worth removing when there has actually been a zip —
        # doing it unconditionally would force a fresh 145 MB every run.
        rm -f "$target_bin_dir/geoip.dat" "$target_bin_dir/geosite.dat"
        if [[ -f "$target_bin_dir/xray" ]]; then
            mv -f "$target_bin_dir/xray" "$xray_bin"
            chmod +x "$xray_bin"
        fi
    fi

    echo -e "${green}Checking geo data...${plain}"
    fetch_geo_file "$target_bin_dir/geoip.dat" \
        "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat" \
        "geoip.dat (Loyalsoldier)"
    fetch_geo_file "$target_bin_dir/geosite.dat" \
        "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat" \
        "geosite.dat (Loyalsoldier)"
    fetch_geo_file "$target_bin_dir/geoip_IR.dat" \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geoip.dat" \
        "geoip_IR.dat (Iran rules)"
    fetch_geo_file "$target_bin_dir/geosite_IR.dat" \
        "https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download/geosite.dat" \
        "geosite_IR.dat (Iran rules)"
    fetch_geo_file "$target_bin_dir/geoip_RU.dat" \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geoip.dat" \
        "geoip_RU.dat (Russia rules)"
    fetch_geo_file "$target_bin_dir/geosite_RU.dat" \
        "https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download/geosite.dat" \
        "geosite_RU.dat (Russia rules)"
    return 0
}

# Wrapper around download_xray_and_geo that, in debug mode only, reuses
# an existing xray + geo bundle from well-known cache locations
# (${xui_folder}/bin, $SCRIPT_DIR/build/bin, $SCRIPT_DIR/target/bin) without
# going near the network at all. A normal update still calls through, where
# the pinned xray version and the geo files' timestamps decide what, if
# anything, is actually worth fetching.
fetch_xray_bundle_smart() {
    local target_bin_dir="$1"
    local panel_fname
    panel_fname=$(xray_panel_arch)

    if [[ "${XUI_DEBUG_MODE:-}" == "1" && -n "$panel_fname" ]]; then
        if [[ -f "$target_bin_dir/xray-linux-${panel_fname}" ]]; then
            echo -e "${green}xray-core + geo data already in ${target_bin_dir}, skipping download.${plain}"
            return 0
        fi
        local src_dir
        for src_dir in "$SCRIPT_DIR/build/bin" "$SCRIPT_DIR/target/bin"; do
            if [[ -f "$src_dir/xray-linux-${panel_fname}" ]]; then
                echo -e "${green}Reusing xray + geo bundle from ${src_dir}.${plain}"
                mkdir -p "$target_bin_dir"
                cp -f "$src_dir"/* "$target_bin_dir/" 2>/dev/null || true
                return 0
            fi
        done
    fi

    download_xray_and_geo "$target_bin_dir"
}

# Ensures a Go toolchain ≥ 1.21 is on PATH. With Go ≥ 1.21 the GOTOOLCHAIN=auto
# default makes `go build` self-bootstrap the version pinned in go.mod, so we
# only need a recent-enough bootstrap here.
ensure_go() {
    local need_install=1
    if _command_exists go; then
        local v
        v=$(go env GOVERSION 2>/dev/null | sed -E 's/^go//')
        if [[ -n "$v" ]]; then
            local min_version="1.21.0"
            if [[ "$(printf '%s\n' "$min_version" "$v" | sort -V | head -n1)" == "$min_version" ]]; then
                need_install=0
            fi
        fi
    fi

    if [[ $need_install -eq 0 ]]; then
        echo -e "${green}Existing Go toolchain detected: $(go env GOVERSION 2>/dev/null)${plain}"
        return 0
    fi

    local goarch
    case "$(arch)" in
        amd64)        goarch="amd64" ;;
        386)          goarch="386" ;;
        arm64)        goarch="arm64" ;;
        armv6|armv7)  goarch="armv6l" ;;
        s390x)        goarch="s390x" ;;
        *)
            echo -e "${red}No prebuilt Go binary for arch $(arch).${plain}"
            return 1
            ;;
    esac

    local go_version="1.26.2"
    local go_url="https://go.dev/dl/go${go_version}.linux-${goarch}.tar.gz"
    local tmp_tgz="/tmp/go-bootstrap.$$.tar.gz"

    if ! check_url_or_skip "$go_url" "Go ${go_version} bootstrap"; then
        return 1
    fi
    echo -e "${green}Installing Go ${go_version} from ${go_url}...${plain}"
    if ! ${curl_bin} -4fLRo "$tmp_tgz" "$go_url"; then
        rm -f "$tmp_tgz"
        echo -e "${red}Failed to download Go ${go_version}.${plain}"
        return 1
    fi
    rm -rf /usr/local/go
    if ! tar -C /usr/local -xzf "$tmp_tgz"; then
        rm -f "$tmp_tgz"
        echo -e "${red}Failed to extract Go ${go_version}.${plain}"
        return 1
    fi
    rm -f "$tmp_tgz"
    export PATH="/usr/local/go/bin:$PATH"
    if ! _command_exists go; then
        echo -e "${red}Go installed but not on PATH.${plain}"
        return 1
    fi
    echo -e "${green}Go installed: $(go version)${plain}"
    return 0
}

# Builds the panel binary from the local source tree and assembles the same
# directory layout the GitHub release tarball would extract into. After this
# returns successfully, update_x-ui's existing post-extract logic (chmod,
# service install, etc.) takes over unchanged with CWD = ${xui_folder}.
update_x-ui_from_source() {
    echo -e "${green}Local source detected at ${SCRIPT_DIR} — building from source...${plain}"
    if ! ensure_go; then
        echo -e "${yellow}Falling back to GitHub release download.${plain}"
        return 1
    fi

    local build_version
    build_version=$(cd "$SCRIPT_DIR" && git describe --tags --always --dirty 2>/dev/null)
    if [[ -z "$build_version" ]]; then
        build_version="v$(cat "$SCRIPT_DIR/config/version" 2>/dev/null || echo unknown)"
    fi
    echo -e "${green}Building x-ui (version ${build_version})...${plain}"

    (cd "$SCRIPT_DIR" && \
     GOTOOLCHAIN=auto CGO_ENABLED=1 go build \
         -ldflags "-w -s -X 'github.com/coinman-dev/3ax-ui/v2/config.version=${build_version}'" \
         -o "$SCRIPT_DIR/build/x-ui" main.go) || {
        echo -e "${red}go build failed — falling back to GitHub release.${plain}"
        return 1
    }

    # Replace only the files we own. x-ui.db (panel database) and bin/
    # (xray + geo data) survive across updates so we don't wipe user data
    # or trigger pointless multi-MB redownloads.
    mkdir -p "${xui_folder}/bin"
    rm -f "${xui_folder}/x-ui" \
          "${xui_folder}/x-ui.sh" \
          "${xui_folder}/x-ui.service" \
          "${xui_folder}/x-ui.service.debian" \
          "${xui_folder}/x-ui.service.arch" \
          "${xui_folder}/x-ui.service.rhel" \
          "${xui_folder}/x-ui.rc"
    cp -f "$SCRIPT_DIR/build/x-ui"           "${xui_folder}/x-ui"
    cp -f "$SCRIPT_DIR/x-ui.sh"              "${xui_folder}/x-ui.sh"
    [[ -f "$SCRIPT_DIR/x-ui.service.debian" ]] && cp -f "$SCRIPT_DIR/x-ui.service.debian" "${xui_folder}/"
    [[ -f "$SCRIPT_DIR/x-ui.service.arch"   ]] && cp -f "$SCRIPT_DIR/x-ui.service.arch"   "${xui_folder}/"
    [[ -f "$SCRIPT_DIR/x-ui.service.rhel"   ]] && cp -f "$SCRIPT_DIR/x-ui.service.rhel"   "${xui_folder}/"
    [[ -f "$SCRIPT_DIR/x-ui.rc"             ]] && cp -f "$SCRIPT_DIR/x-ui.rc"             "${xui_folder}/"

    if ! fetch_xray_bundle_smart "${xui_folder}/bin"; then
        echo -e "${red}Failed to fetch xray-core for the local-source update.${plain}"
        return 1
    fi

    tag_version="${build_version}"
    return 0
}

update_x-ui() {
    cd ${xui_folder%/x-ui}/
    local xray_backup=""
    local xray_backup_name=""

    if [ -f "${xui_folder}/x-ui" ]; then
        current_xui_version=$(${xui_folder}/x-ui -v)
        echo -e "${green}Current x-ui version: ${current_xui_version}${plain}"
    else
        _fail "ERROR: Current x-ui version: unknown"
    fi

    # Local-source update path — build from cloned repo, skip the GitHub
    # download. Mirrors the GitHub-release flow's pre-/post-install hooks
    # (xray binary preservation, service-unit reinstall, x-ui.sh reinstall,
    # owner / permission fixups, config_after_update).
    if is_local_source_install; then
        echo -e "${green}Preserving xray binary before update...${plain}"
        if [[ -e ${xui_folder}/ ]]; then
            for candidate in "${xui_folder}"/bin/xray-linux-*; do
                if [[ -f "$candidate" ]]; then
                    xray_backup_name=$(basename "$candidate")
                    xray_backup="/tmp/${xray_backup_name}.xui-update.$$"
                    if cp -f "$candidate" "$xray_backup" >/dev/null 2>&1; then
                        echo -e "${green}Preserving existing Xray core binary: ${xray_backup_name}${plain}"
                    else
                        xray_backup=""
                        xray_backup_name=""
                    fi
                    break
                fi
            done

            echo -e "${green}Stopping x-ui...${plain}"
            if [[ $release == "alpine" ]]; then
                rc-service x-ui stop >/dev/null 2>&1
                rc-update del x-ui >/dev/null 2>&1
                rm -f /etc/init.d/x-ui >/dev/null 2>&1
            else
                systemctl stop x-ui >/dev/null 2>&1
                systemctl disable x-ui >/dev/null 2>&1
                rm ${xui_service}/x-ui.service -f >/dev/null 2>&1
                systemctl daemon-reload >/dev/null 2>&1
            fi
        fi

        if update_x-ui_from_source; then
            cd "${xui_folder}" >/dev/null 2>&1
            chmod +x x-ui >/dev/null 2>&1
            if [[ $(arch) == "armv5" || $(arch) == "armv6" || $(arch) == "armv7" ]]; then
                mv bin/xray-linux-$(arch) bin/xray-linux-arm >/dev/null 2>&1
                chmod +x bin/xray-linux-arm >/dev/null 2>&1
            fi
            chmod +x x-ui >/dev/null 2>&1
            [ -f bin/xray-linux-$(arch) ] && chmod +x bin/xray-linux-$(arch) >/dev/null 2>&1
            # Only if it went missing. bin/ is not wiped on this path, and
            # the fetch above leaves a current xray where it is and replaces
            # an outdated one — copying the old binary back over either would
            # undo the upgrade this run just made.
            if [[ -n "$xray_backup" && -n "$xray_backup_name" && -f "$xray_backup" ]]; then
                if [[ ! -f "bin/${xray_backup_name}" ]]; then
                    cp -f "$xray_backup" "bin/${xray_backup_name}" >/dev/null 2>&1 && \
                        chmod +x "bin/${xray_backup_name}" >/dev/null 2>&1
                fi
                rm -f "$xray_backup" >/dev/null 2>&1
            fi
            # Ensure the mtg MTProto sidecar is present (the local-source build
            # only fetches xray). No-op if already installed; non-fatal.
            install_mtg "${xui_folder}/bin"

            cp -f "${xui_folder}/x-ui.sh" /usr/bin/x-ui >/dev/null 2>&1
            chmod +x ${xui_folder}/x-ui.sh >/dev/null 2>&1
            chmod +x /usr/bin/x-ui >/dev/null 2>&1
            mkdir -p /var/log/x-ui >/dev/null 2>&1
            chown -R root:root ${xui_folder} >/dev/null 2>&1
            [ -f "${xui_folder}/bin/config.json" ] && chmod 640 ${xui_folder}/bin/config.json >/dev/null 2>&1

            update_x-ui_install_service
            config_after_update
            update_x-ui_print_footer
            return
        fi
        echo -e "${yellow}Local-source update did not complete — falling back to GitHub release.${plain}"
        # Fall through to the GitHub-release flow.
    fi

    echo -e "${green}Downloading new x-ui version...${plain}"

    if [[ "$1" == "--beta" || "$1" == "--pre" ]]; then
        tag_version=$(${curl_bin} -4 -Ls "https://api.github.com/repos/${XUI_REPO}/releases" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' | head -1)
        if [[ ! -n "$tag_version" ]]; then
            echo -e "${yellow}Retrying over dual-stack...${plain}"
            tag_version=$(${curl_bin} -Ls "https://api.github.com/repos/${XUI_REPO}/releases" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' | head -1)
        fi
        echo -e "Got x-ui latest pre-release version: ${tag_version}, beginning the installation..."
    elif [[ -n "$1" ]]; then
        # An explicit tag, positional exactly as install.sh takes one — e.g.
        # forwarded by check_existing_install() when it hands a non-TTY caller
        # over to us rather than silently swapping in the latest release. Same
        # floor as install.sh's tagged install: this fork's own releases start
        # at v1.0.0, and the 2.3.5 floor inherited from upstream 3x-ui belongs to
        # its numbering, not ours.
        tag_version="$1"
        tag_version_numeric=${tag_version#v}
        local min_version="1.0.0"
        if [[ "$(printf '%s\n' "$min_version" "$tag_version_numeric" | sort -V | head -n1)" != "$min_version" ]]; then
            _fail "ERROR: Please use a newer version (at least v${min_version}). Exiting update."
        fi
        echo -e "Updating to the requested version: ${tag_version}..."
    else
        tag_version=$(${curl_bin} -4 -Ls "https://api.github.com/repos/${XUI_REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        if [[ ! -n "$tag_version" ]]; then
            echo -e "${yellow}Retrying over dual-stack...${plain}"
            tag_version=$(${curl_bin} -Ls "https://api.github.com/repos/${XUI_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
        fi
        echo -e "Got x-ui latest version: ${tag_version}, beginning the installation..."
    fi
    if [[ ! -n "$tag_version" ]]; then
        _fail "ERROR: Failed to fetch x-ui version, it may be due to GitHub API restrictions, please try it later"
    fi
    # IPv4 first, dual-stack second. On a box with no global IPv6 the dual-stack
    # attempt burns curl's whole connect timeout before it falls back, and this
    # order used to be the other way round.
    ${curl_bin} -4fLRo ${xui_folder}-linux-$(arch).tar.gz "https://github.com/${XUI_REPO}/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz" 2>/dev/null
    if [[ $? -ne 0 ]]; then
        echo -e "${yellow}Retrying over dual-stack...${plain}"
        ${curl_bin} -fLRo ${xui_folder}-linux-$(arch).tar.gz "https://github.com/${XUI_REPO}/releases/download/${tag_version}/x-ui-linux-$(arch).tar.gz" 2>/dev/null
        if [[ $? -ne 0 ]]; then
            _fail "ERROR: Failed to download x-ui, please be sure that your server can access GitHub"
        fi
    fi
    
    # Verify the archive BEFORE anything is removed. An interrupted download
    # used to reach the install step below, where tar failed with its output
    # discarded and `cd x-ui` silently did nothing — the old version was already
    # deleted by then, so the panel was left with no binary at all and every
    # later step reported "No such file or directory".
    if ! tar -tzf x-ui-linux-$(arch).tar.gz >/dev/null 2>&1; then
        rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
        _fail "ERROR: the downloaded archive is corrupt (interrupted download?). Nothing has been changed — run the update again."
    fi
    if ! tar -tzf x-ui-linux-$(arch).tar.gz 2>/dev/null | grep -qx "x-ui/x-ui"; then
        rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
        _fail "ERROR: the downloaded archive does not contain the x-ui binary. Nothing has been changed."
    fi

    # Stage the management wrapper while the box is still whole. It ships in the
    # tarball, so the usual update needs no second download at all; only a
    # tarball that predates it falls back to the raw file, and that fetch has to
    # happen here — below, the service is already stopped and its unit gone, and
    # a 404 there is what cost the stand its front.
    wrapper_staged=""
    if ! tar -tzf "x-ui-linux-$(arch).tar.gz" 2>/dev/null | grep -qx "x-ui/x-ui.sh"; then
        echo -e "${yellow}This release tarball ships no x-ui.sh — fetching the management wrapper from ${REPO_BRANCH}...${plain}"
        wrapper_staged="/tmp/x-ui.sh.xui-update.$$"
        ${curl_bin} -4fLRo "${wrapper_staged}" "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.sh" >/dev/null 2>&1 ||
            ${curl_bin} -fLRo "${wrapper_staged}" "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.sh" >/dev/null 2>&1
        if [[ ! -s "${wrapper_staged}" ]]; then
            rm -f "${wrapper_staged}" >/dev/null 2>&1
            rm -f "x-ui-linux-$(arch).tar.gz" >/dev/null 2>&1
            _fail "ERROR: failed to download x-ui.sh from ${XUI_REPO}@${REPO_BRANCH}. Nothing has been changed — the box keeps running on the current version."
        fi
    fi

    if [[ -e ${xui_folder}/ ]]; then
        for candidate in "${xui_folder}"/bin/xray-linux-*; do
            if [[ -f "$candidate" ]]; then
                xray_backup_name=$(basename "$candidate")
                xray_backup="/tmp/${xray_backup_name}.xui-update.$$"
                if cp -f "$candidate" "$xray_backup" >/dev/null 2>&1; then
                    echo -e "${green}Preserving existing Xray core binary: ${xray_backup_name}${plain}"
                else
                    xray_backup=""
                    xray_backup_name=""
                    echo -e "${yellow}Failed to preserve existing Xray core binary; bundled core may be used after update.${plain}"
                fi
                break
            fi
        done

        echo -e "${green}Stopping x-ui...${plain}"
        if [[ $release == "alpine" ]]; then
            if [ -f "/etc/init.d/x-ui" ]; then
                rc-service x-ui stop >/dev/null 2>&1
                rc-update del x-ui >/dev/null 2>&1
                echo -e "${green}Removing old service unit version...${plain}"
                rm -f /etc/init.d/x-ui >/dev/null 2>&1
            else
                rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
                _fail "ERROR: x-ui service unit not installed."
            fi
        else
            if [ -f "${xui_service}/x-ui.service" ]; then
                systemctl stop x-ui >/dev/null 2>&1
                systemctl disable x-ui >/dev/null 2>&1
                echo -e "${green}Removing old systemd unit version...${plain}"
                rm ${xui_service}/x-ui.service -f >/dev/null 2>&1
                systemctl daemon-reload >/dev/null 2>&1
            else
                rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
                _fail "ERROR: x-ui systemd unit not installed."
            fi
        fi
        echo -e "${green}Removing old x-ui version...${plain}"
        rm ${xui_folder} -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui.service -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui.service.debian -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui.service.arch -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui.service.rhel -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui -f >/dev/null 2>&1
        rm ${xui_folder}/x-ui.sh -f >/dev/null 2>&1
        echo -e "${green}Removing old xray version...${plain}"
        rm ${xui_folder}/bin/xray-linux-amd64 -f >/dev/null 2>&1
        echo -e "${green}Removing old README and LICENSE file...${plain}"
        rm ${xui_folder}/bin/README.md -f >/dev/null 2>&1
        rm ${xui_folder}/bin/LICENSE -f >/dev/null 2>&1
    else
        rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
        _fail "ERROR: x-ui not installed."
    fi
    
    echo -e "${green}Installing new x-ui version...${plain}"
    # Both steps are load-bearing and used to fail silently: the old version is
    # gone at this point, so anything that goes wrong here has to say so.
    if ! tar zxf x-ui-linux-$(arch).tar.gz >/dev/null 2>&1; then
        _fail_after_stop "ERROR: failed to unpack x-ui-linux-$(arch).tar.gz. The panel binary is missing — run the update again to restore it."
    fi
    rm x-ui-linux-$(arch).tar.gz -f >/dev/null 2>&1
    cd x-ui || _fail_after_stop "ERROR: the unpacked x-ui folder is missing. The panel binary is missing — run the update again to restore it."
    chmod +x x-ui >/dev/null 2>&1
    
    # Check the system's architecture and rename the file accordingly
    if [[ $(arch) == "armv5" || $(arch) == "armv6" || $(arch) == "armv7" ]]; then
        mv bin/xray-linux-$(arch) bin/xray-linux-arm >/dev/null 2>&1
        chmod +x bin/xray-linux-arm >/dev/null 2>&1
        mv bin/mtg-linux-$(arch) bin/mtg-linux-arm >/dev/null 2>&1
        chmod +x bin/mtg-linux-arm >/dev/null 2>&1
    fi

    chmod +x x-ui >/dev/null 2>&1
    [ -f bin/xray-linux-$(arch) ] && chmod +x bin/xray-linux-$(arch) >/dev/null 2>&1
    # Ensure the mtg MTProto sidecar is present (covers updates from a tarball
    # that predates MTProto support). No-op if already shipped; non-fatal.
    install_mtg "bin"
    if [[ -n "$xray_backup" && -n "$xray_backup_name" && -f "$xray_backup" ]]; then
        cp -f "$xray_backup" "bin/${xray_backup_name}" >/dev/null 2>&1
        if [[ $? -eq 0 ]]; then
            chmod +x "bin/${xray_backup_name}" >/dev/null 2>&1
            echo -e "${green}Restored existing Xray core binary: ${xray_backup_name}${plain}"
        else
            echo -e "${yellow}Failed to restore existing Xray core binary; using bundled version.${plain}"
        fi
        rm -f "$xray_backup" >/dev/null 2>&1
    fi
    
    echo -e "${green}Installing the x-ui.sh management script...${plain}"
    # Whatever the tarball delivered wins — it is the wrapper that matches this
    # binary. Otherwise the copy staged before the stop. Nothing is downloaded
    # here: this side of the stop, a failed fetch must not end the run.
    if [[ -s x-ui.sh ]]; then
        cp -f x-ui.sh /usr/bin/x-ui >/dev/null 2>&1
    elif [[ -n "${wrapper_staged}" && -s "${wrapper_staged}" ]]; then
        cp -f "${wrapper_staged}" /usr/bin/x-ui >/dev/null 2>&1
    else
        echo -e "${yellow}WARNING: no x-ui.sh to install — the 'x-ui' command keeps its previous version. The service itself is unaffected.${plain}"
    fi
    rm -f "${wrapper_staged}" >/dev/null 2>&1
    
    chmod +x ${xui_folder}/x-ui.sh >/dev/null 2>&1
    chmod +x /usr/bin/x-ui >/dev/null 2>&1
    mkdir -p /var/log/x-ui >/dev/null 2>&1
    
    echo -e "${green}Changing owner...${plain}"
    chown -R root:root ${xui_folder} >/dev/null 2>&1
    
    if [ -f "${xui_folder}/bin/config.json" ]; then
        echo -e "${green}Changing on config file permissions...${plain}"
        chmod 640 ${xui_folder}/bin/config.json >/dev/null 2>&1
    fi
    
    update_x-ui_install_service
    config_after_update
    update_x-ui_print_footer
}

# Installs and starts the OS service unit during update. Prefers files
# embedded in ${xui_folder}/ (delivered both by the release tarball and by
# the local-source build); falls back to GitHub raw if missing.
# Regenerates the proxy-front service unit on update (panel boxes use the
# templated unit shipped in the tarball instead).
write_proxy_service_unit() {
    if [[ $release == "alpine" ]]; then
        cat >/etc/init.d/x-ui <<EOF
#!/sbin/openrc-run
command="${xui_folder}/x-ui"
command_args="proxy -c /etc/x-ui/proxy.json"
command_background=true
pidfile="/run/x-ui.pid"
description="x-ui proxy front"
procname="x-ui"
depend() {
    need net
}
start_pre(){
    cd ${xui_folder}
}
EOF
        chmod +x /etc/init.d/x-ui >/dev/null 2>&1
        rc-update add x-ui >/dev/null 2>&1
        rc-service x-ui restart >/dev/null 2>&1
        return
    fi
    echo -e "${green}Installing proxy-front systemd unit...${plain}"
    cat >${xui_service}/x-ui.service <<EOF
[Unit]
Description=x-ui (proxy front)
After=network.target
Wants=network.target

[Service]
Type=simple
WorkingDirectory=${xui_folder}/
ExecStart=${xui_folder}/x-ui proxy -c /etc/x-ui/proxy.json
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
    chown root:root ${xui_service}/x-ui.service >/dev/null 2>&1
    chmod 644 ${xui_service}/x-ui.service >/dev/null 2>&1
    systemctl daemon-reload >/dev/null 2>&1
    systemctl enable x-ui >/dev/null 2>&1
    systemctl restart x-ui >/dev/null 2>&1
}

update_x-ui_install_service() {
    if [[ "${XUI_PROXY_MODE:-}" == "1" ]]; then
        write_proxy_service_unit
        return
    fi
    if [[ $release == "alpine" ]]; then
        if [ -f "${xui_folder}/x-ui.rc" ]; then
            cp -f "${xui_folder}/x-ui.rc" /etc/init.d/x-ui >/dev/null 2>&1
        else
            ${curl_bin} -4fLRo /etc/init.d/x-ui "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.rc" >/dev/null 2>&1
            if [[ $? -ne 0 ]]; then
                ${curl_bin} -fLRo /etc/init.d/x-ui "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.rc" >/dev/null 2>&1
                [[ $? -ne 0 ]] && _fail "ERROR: Failed to download startup unit x-ui.rc"
            fi
        fi
        chmod +x /etc/init.d/x-ui >/dev/null 2>&1
        chown root:root /etc/init.d/x-ui >/dev/null 2>&1
        rc-update add x-ui >/dev/null 2>&1
        rc-service x-ui start >/dev/null 2>&1
        return
    fi

    # systemd path
    local service_installed=false
    if [ -f "${xui_folder}/x-ui.service" ]; then
        echo -e "${green}Installing systemd unit...${plain}"
        cp -f "${xui_folder}/x-ui.service" ${xui_service}/ >/dev/null 2>&1 && service_installed=true
    fi
    if [ "$service_installed" = false ]; then
        case "${release}" in
            ubuntu | debian | armbian)
                if [ -f "${xui_folder}/x-ui.service.debian" ]; then
                    echo -e "${green}Installing debian-like systemd unit...${plain}"
                    cp -f "${xui_folder}/x-ui.service.debian" ${xui_service}/x-ui.service >/dev/null 2>&1 && service_installed=true
                fi
            ;;
            arch | manjaro | parch)
                if [ -f "${xui_folder}/x-ui.service.arch" ]; then
                    echo -e "${green}Installing arch-like systemd unit...${plain}"
                    cp -f "${xui_folder}/x-ui.service.arch" ${xui_service}/x-ui.service >/dev/null 2>&1 && service_installed=true
                fi
            ;;
            *)
                if [ -f "${xui_folder}/x-ui.service.rhel" ]; then
                    echo -e "${green}Installing rhel-like systemd unit...${plain}"
                    cp -f "${xui_folder}/x-ui.service.rhel" ${xui_service}/x-ui.service >/dev/null 2>&1 && service_installed=true
                fi
            ;;
        esac
    fi
    if [ "$service_installed" = false ]; then
        echo -e "${yellow}Service files not found locally, downloading from GitHub...${plain}"
        case "${release}" in
            ubuntu | debian | armbian)
                ${curl_bin} -4fLRo ${xui_service}/x-ui.service "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.service.debian" >/dev/null 2>&1
            ;;
            arch | manjaro | parch)
                ${curl_bin} -4fLRo ${xui_service}/x-ui.service "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.service.arch" >/dev/null 2>&1
            ;;
            *)
                ${curl_bin} -4fLRo ${xui_service}/x-ui.service "https://raw.githubusercontent.com/${XUI_REPO}/${REPO_BRANCH}/x-ui.service.rhel" >/dev/null 2>&1
            ;;
        esac
        [[ $? -ne 0 ]] && _fail "ERROR: Failed to install x-ui.service from GitHub"
    fi
    chown root:root ${xui_service}/x-ui.service >/dev/null 2>&1
    chmod 644 ${xui_service}/x-ui.service >/dev/null 2>&1
    systemctl daemon-reload >/dev/null 2>&1
    systemctl enable x-ui >/dev/null 2>&1
    systemctl start x-ui >/dev/null 2>&1
}

update_x-ui_print_footer() {
    echo -e "${green}x-ui ${tag_version}${plain} updating finished, it is running now..."
    echo -e ""
    echo -e "┌───────────────────────────────────────────────────────┐
│  ${blue}x-ui control menu usages (subcommands):${plain}              │
│                                                       │
│  ${blue}x-ui${plain}              - Admin Management Script          │
│  ${blue}x-ui start${plain}        - Start                            │
│  ${blue}x-ui stop${plain}         - Stop                             │
│  ${blue}x-ui restart${plain}      - Restart                          │
│  ${blue}x-ui status${plain}       - Current Status                   │
│  ${blue}x-ui settings${plain}     - Current Settings                 │
│  ${blue}x-ui enable${plain}       - Enable Autostart on OS Startup   │
│  ${blue}x-ui disable${plain}      - Disable Autostart on OS Startup  │
│  ${blue}x-ui log${plain}          - Check logs                       │
│  ${blue}x-ui banlog${plain}       - Check Fail2ban ban logs          │
│  ${blue}x-ui update${plain}       - Update                           │
│  ${blue}x-ui legacy${plain}       - Legacy version                   │
│  ${blue}x-ui install${plain}      - Install                          │
│  ${blue}x-ui uninstall${plain}    - Uninstall                        │
└───────────────────────────────────────────────────────┘"
}

# ensure_amneziawg_current upgrades the AmneziaWG packages and says so when the
# kernel module and the userspace tools disagree about their generation.
#
# The two halves come from one PPA but not always from one upstream commit: in
# August 2026 the tools shipped 3.1 while the module was still 3.0. The panel
# asks the tools what generation the host supports, wrote the 3.0 parameters
# they advertise, and the module refused them — «awg setconf: Unable to modify
# interface: Invalid argument», with the tunnel down and nothing in the log
# naming the cause. Keeping both current is the only state that is coherent,
# and when they still differ the operator needs to be told in words.
#
# Nothing here is fatal. A server whose PPA is unreachable keeps the AmneziaWG
# it already has.
ensure_amneziawg_current() {
    command -v awg &>/dev/null || return 0
    case "${release}" in
        ubuntu | debian | armbian) ;;
        *) return 0 ;;
    esac

    local pkgs=() p
    for p in amneziawg amneziawg-dkms amneziawg-tools; do
        dpkg -s "$p" &>/dev/null && pkgs+=("$p")
    done
    [[ ${#pkgs[@]} -gt 0 ]] || return 0

    echo -e "${green}Checking AmneziaWG for updates...${plain}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q &>/dev/null || true
    # All of them in one transaction: upgrading the tools without the module is
    # exactly the split this function exists to prevent.
    apt-get install -y -q --only-upgrade "${pkgs[@]}" &>/dev/null || true

    reload_amneziawg_module
    warn_amneziawg_version_split
}

# reload_amneziawg_module swaps in a freshly built module without a reboot.
#
# A DKMS build lands on disk, but the running kernel goes on using the module it
# already loaded — so an upgrade appears to do nothing until the machine is
# restarted. Reloading is only safe while nothing is using it; when a tunnel is
# up we say what is needed instead of tearing it down underneath the operator.
reload_amneziawg_module() {
    local users
    users=$(lsmod 2>/dev/null | awk '$1 == "amneziawg" {print $3}')
    [[ -n "$users" ]] || { modprobe amneziawg &>/dev/null || true; return 0; }

    if [[ "$users" == "0" ]]; then
        modprobe -r amneziawg &>/dev/null && modprobe amneziawg &>/dev/null || true
        return 0
    fi
    echo -e "${yellow}The AmneziaWG module is in use, so the running kernel keeps the old one.${plain}"
    echo -e "${yellow}It is picked up on the next reboot, or after: awg-quick down awg0 && modprobe -r amneziawg && modprobe amneziawg${plain}"
}

# warn_amneziawg_version_split reports a module and tools that are not the same
# generation, which the panel cannot tell from the outside.
warn_amneziawg_version_split() {
    local mod tools mod_gen tools_gen
    mod=$(cat /sys/module/amneziawg/version 2>/dev/null ||
        modinfo -F version amneziawg 2>/dev/null)
    tools=$(awg --version 2>/dev/null | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)
    [[ -n "$mod" && -n "$tools" ]] || return 0

    # Compare major.minor only: the trailing build date moves on its own.
    mod_gen=$(echo "$mod" | cut -d. -f1,2)
    tools_gen=$(echo "$tools" | cut -d. -f1,2)
    [[ "$mod_gen" == "$tools_gen" ]] && return 0

    echo -e "${yellow}AmneziaWG halves are out of step: kernel module ${mod}, tools ${tools}.${plain}"
    echo -e "${yellow}The panel offers whatever the older half supports, so the tunnel keeps working —${plain}"
    echo -e "${yellow}but the newer features stay off until both are on the same generation.${plain}"
}

# prune_stale_amneziawg_dkms drops every amneziawg build in the DKMS tree except
# the newest one.
#
# Upgrading the package normally removes its own predecessor, but a version left
# behind by an interrupted upgrade — or a module someone built by hand before
# the panel was installed — stays in the tree, gets rebuilt for every new kernel
# from then on, and leaves two amneziawg.ko for depmod to choose between. The
# symptom is a tunnel that works until a kernel update and then loads the wrong
# module.
prune_stale_amneziawg_dkms() {
    command -v dkms &>/dev/null || return 0

    local versions count newest
    # dkms 2.x prints "amneziawg, 1.0.0, <kernel>, ..."; 3.x prints
    # "amneziawg/1.0.0, <kernel>, ...". Both reduce to the version alone.
    versions=$(dkms status amneziawg 2>/dev/null |
        sed -E 's#^amneziawg[/,][[:space:]]*([^,]+),.*#\1#' | sort -Vu)
    count=$(echo "$versions" | grep -c '[^[:space:]]')
    [[ "$count" -gt 1 ]] || return 0

    newest=$(echo "$versions" | tail -n1)
    echo -e "${yellow}Several amneziawg versions found in the DKMS tree, keeping ${newest}:${plain}"
    while read -r v; do
        [[ -n "$v" && "$v" != "$newest" ]] || continue
        echo -e "${yellow}  removing amneziawg/${v}${plain}"
        dkms remove "amneziawg/${v}" --all &>/dev/null ||
            dkms remove -m amneziawg -v "${v}" --all &>/dev/null || true
    done <<<"$versions"
    depmod -a &>/dev/null || true
}

ensure_wireguard_native() {
    if command -v wg &>/dev/null; then
        modprobe wireguard 2>/dev/null || true
        return
    fi
    echo -e "${yellow}wireguard-tools not found, installing...${plain}"
    case "${release}" in
        ubuntu | debian | armbian)
            apt-get install -y -q wireguard-tools 2>/dev/null || true
            ;;
        fedora | amzn | rhel | almalinux | rocky | ol | centos)
            dnf install -y wireguard-tools 2>/dev/null || yum install -y wireguard-tools 2>/dev/null || true
            ;;
        arch | manjaro | parch)
            pacman -Syu --noconfirm wireguard-tools 2>/dev/null || true
            ;;
        alpine)
            apk add wireguard-tools 2>/dev/null || true
            ;;
    esac
    modprobe wireguard 2>/dev/null || true
}

echo -e "${green}Running...${plain}"
# Detects whether the existing install is in debug / localhost-only mode
# and inherits that setting so the update doesn't surprise the user with
# new prompts. Heuristic: panel binds to 127.0.0.1 AND no SSL cert is
# configured. Env override XUI_DEBUG_MODE=1 always wins; in that case
# XUI_DEBUG_PORT keeps whatever was either passed in env or pre-existing.
detect_debug_mode_from_existing_install() {
    if [[ "${XUI_DEBUG_MODE:-}" == "1" ]]; then
        echo -e "${yellow}Debug mode forced via XUI_DEBUG_MODE=1.${plain}"
    elif [[ -x "${xui_folder}/x-ui" ]]; then
        local existing_listen existing_cert
        existing_listen=$(${xui_folder}/x-ui setting -getListen true 2>/dev/null | grep -Eo 'listenIP: .*' | awk '{print $2}')
        existing_cert=$(${xui_folder}/x-ui setting -getCert true 2>/dev/null | grep 'cert:' | awk -F': ' '{print $2}' | tr -d '[:space:]')
        if [[ "${existing_listen}" == "127.0.0.1" && -z "${existing_cert}" ]]; then
            export XUI_DEBUG_MODE=1
            echo -e "${yellow}Detected debug / localhost-only install (listenIP=127.0.0.1, no SSL) — continuing in debug mode.${plain}"
        else
            export XUI_DEBUG_MODE=0
        fi
    else
        export XUI_DEBUG_MODE=0
    fi

    # Carry the existing port forward in debug mode so the update keeps
    # the same URL the user is already using.
    if [[ "${XUI_DEBUG_MODE}" == "1" ]]; then
        if [[ -z "${XUI_DEBUG_PORT:-}" ]]; then
            local existing_port
            existing_port=$(${xui_folder}/x-ui setting -show true 2>/dev/null | grep -Eo 'port: .+' | awk '{print $2}')
            if [[ -n "${existing_port}" && "${existing_port}" != "0" ]]; then
                export XUI_DEBUG_PORT="${existing_port}"
            else
                export XUI_DEBUG_PORT=8080
            fi
        fi
    fi
}

# Decides whether this release may touch the box at all (spec §5.7).
#
# A v1 proxy.json — one carrying a key from the manifest era, or lacking the v2
# marker — belongs to a box that cannot run as a chain hop: its ports, its next
# hop and its secret all used to come from places this release no longer reads.
# The check lives here, before the binary is replaced, and not in
# config_after_update where its ancestor sat: by then "we stopped" would mean a
# dead relay restarting in a loop, whereas here it means the box keeps serving
# its clients on the version it already runs until its owner reinstalls it.
proxy_config_gate() {
    local cfg="${1:-/etc/x-ui/proxy.json}" key
    local hint="this release runs proxy fronts as chain hops. The box keeps running on the current binary. Re-install it as a chain hop: docs/runbooks/proxy-front.md §«Переустановка бокса»."
    for key in upstreamHost relayManifestPath extraPorts upstreamBase subPath jsonPath; do
        if grep -q "\"${key}\"" "${cfg}" 2>/dev/null; then
            echo -e "${red}${cfg} is a v1 proxy-front config (key \"${key}\"): ${hint}${plain}"
            exit 1
        fi
    done
    if ! grep -Eq '"version"[[:space:]]*:[[:space:]]*2' "${cfg}" 2>/dev/null; then
        echo -e "${red}${cfg} carries no \"version\": 2 marker, so it is a v1 proxy-front config: ${hint}${plain}"
        exit 1
    fi
    # A v2 box with no hop secret is fine — it simply has not joined yet.
    if ! grep -Eq '"hopSecret"[[:space:]]*:[[:space:]]*"[^"]+"' "${cfg}" 2>/dev/null; then
        proxy_not_joined=1
    fi
}

# Proxy-front boxes carry /etc/x-ui/proxy.json; update them in proxy mode
# (replace the binary, keep proxy.json, /etc/x-ui/chain/, the certificate and
# the proxy unit) and skip the panel / WireGuard steps.
if [[ -f /etc/x-ui/proxy.json ]]; then
    export XUI_PROXY_MODE=1
    echo -e "${yellow}Detected proxy-front install (/etc/x-ui/proxy.json) — updating in proxy mode.${plain}"
    proxy_config_gate
fi
if [[ "${XUI_PROXY_MODE:-}" != "1" ]]; then
    detect_debug_mode_from_existing_install
fi
install_base
if [[ "${XUI_PROXY_MODE:-}" != "1" ]]; then
    ensure_wireguard_native
    ensure_amneziawg_current
    prune_stale_amneziawg_dkms
fi
# Hops too: port 80 belongs to nginx on every box (acme_front_setup) — but on a
# hop only where nothing else holds it, and without the package starting it.
if [[ "${XUI_PROXY_MODE:-}" == "1" ]]; then
    hop_install_nginx
else
    install_nginx
fi
update_x-ui $1
