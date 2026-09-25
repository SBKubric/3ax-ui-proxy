package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

// Port 80 belongs to nginx on every box (ADR 0005, #138): acme.sh no longer
// listens there itself but drops the challenge into the webroot nginx serves.
// These tests drive the shell side of that with a stub acme.sh, a stub x-ui
// binary and a stub nginx — nothing on the machine is touched.

// certScripts are the three scripts that issue certificates. Each carries its
// own copy of the helpers, because each is run on its own: install.sh piped
// from curl, update.sh the same, x-ui.sh as /usr/bin/x-ui.
var certScripts = []string{"install.sh", "update.sh", "x-ui.sh"}

// issueFunctions is what acme_issue_webroot needs to run.
var issueFunctions = []string{
	"acme_webroot", "acme_nginx_reload_cmd", "acme_reload_cmd", "acme_front_ready",
	"acme_issue_webroot", "acme_ip_flags", "is_ip", "is_ipv4", "is_ipv6",
}

// acmeStubs lays out a fake root: acme.sh under $HOME that logs its arguments
// and installs a certificate on --installcert, an x-ui binary that logs the
// nginx acme-front call (failing it when frontFails), and nginx on PATH.
func acmeStubs(t *testing.T, frontFails bool) (env []string, home, xuiFolder, logFile string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "root")
	xuiFolder = filepath.Join(root, "x-ui")
	bin := filepath.Join(root, "bin")
	logFile = filepath.Join(root, "calls.log")
	for _, dir := range []string{filepath.Join(home, ".acme.sh"), xuiFolder, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	acme := `#!/bin/bash
echo "acme.sh $*" >>"` + logFile + `"
if [[ "$1" == "--installcert" ]]; then
    while [[ $# -gt 0 ]]; do
        case "$1" in
        --key-file) echo key >"$2"; shift ;;
        --fullchain-file) echo cert >"$2"; shift ;;
        esac
        shift
    done
fi
exit 0
`
	front := "0"
	if frontFails {
		front = "1"
	}
	xui := `#!/bin/bash
echo "x-ui $*" >>"` + logFile + `"
exit ` + front + "\n"
	for path, body := range map[string]string{
		filepath.Join(home, ".acme.sh", "acme.sh"): acme,
		filepath.Join(xuiFolder, "x-ui"):           xui,
		filepath.Join(bin, "nginx"):                "#!/bin/bash\nexit 0\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env = []string{"HOME=" + home, "xui_folder=" + xuiFolder, "PATH=" + bin + ":" + os.Getenv("PATH")}
	return env, home, xuiFolder, logFile
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// TestACMEIssueWebrootIssuesThroughNginx: the one issuing path of panel and
// hops alike. nginx is put on port 80 first, then acme.sh is asked for a
// webroot issuance — never standalone — and installs the certificate with a
// reload command that reloads nginx as well as restarting x-ui.
func TestACMEIssueWebrootIssuesThroughNginx(t *testing.T) {
	for _, script := range certScripts {
		t.Run(script, func(t *testing.T) {
			env, _, _, logFile := acmeStubs(t, false)
			certDir := filepath.Join(t.TempDir(), "ip")

			out, err := runScriptShell(t, script, issueFunctions,
				"install_nginx() { echo install_nginx; }\nacme_issue_webroot '"+certDir+"' 3 '' 203.0.113.5\n", env...)
			if err != nil {
				t.Fatalf("acme_issue_webroot failed: %v\n%s", err, out)
			}
			calls := readLog(t, logFile)
			front := strings.Index(calls, "x-ui nginx acme-front")
			issue := strings.Index(calls, "acme.sh --issue")
			if front < 0 || issue < 0 || front > issue {
				t.Fatalf("nginx was not put on port 80 before the issuance:\n%s", calls)
			}
			issueLine := lineWith(calls, "acme.sh --issue")
			for _, want := range []string{
				"-d 203.0.113.5", "--webroot " + nginx.ACMEWebroot, "--server letsencrypt",
				"--certificate-profile shortlived", "--days 3", "--request-v4",
			} {
				if !strings.Contains(issueLine, want) {
					t.Errorf("the issuance lacks %q: %s", want, issueLine)
				}
			}
			for _, banned := range []string{"--standalone", "--httpport"} {
				if strings.Contains(issueLine, banned) {
					t.Errorf("the issuance still asks for %s: %s", banned, issueLine)
				}
			}
			install := lineWith(calls, "acme.sh --installcert")
			if !strings.Contains(install, "-d 203.0.113.5") || !strings.Contains(install, "--fullchain-file "+certDir+"/fullchain.pem") {
				t.Errorf("the certificate is not installed into %s: %s", certDir, install)
			}
			if !strings.Contains(install, "reload nginx") || !strings.Contains(install, "nginx -s reload") || !strings.Contains(install, "restart x-ui") {
				t.Errorf("the reload command does not reload nginx and restart x-ui: %s", install)
			}
			if _, err := os.Stat(filepath.Join(certDir, "privkey.pem")); err != nil {
				t.Errorf("no key in %s: %v", certDir, err)
			}
		})
	}
}

// TestACMEIssueWebrootForADomain: a domain certificate is an ordinary one —
// no short-lived profile, no --days unless asked — and keeps a reload command
// the operator chose.
func TestACMEIssueWebrootForADomain(t *testing.T) {
	for _, script := range certScripts {
		t.Run(script, func(t *testing.T) {
			env, _, _, logFile := acmeStubs(t, false)
			certDir := filepath.Join(t.TempDir(), "vpn.example.com")
			out, err := runScriptShell(t, script, issueFunctions,
				"acme_issue_webroot '"+certDir+"' '' 'my reload' vpn.example.com\n", env...)
			if err != nil {
				t.Fatalf("acme_issue_webroot failed: %v\n%s", err, out)
			}
			calls := readLog(t, logFile)
			issueLine := lineWith(calls, "acme.sh --issue")
			if !strings.Contains(issueLine, "-d vpn.example.com") || !strings.Contains(issueLine, "--webroot "+nginx.ACMEWebroot) {
				t.Errorf("issuance: %s", issueLine)
			}
			if strings.Contains(issueLine, "shortlived") || strings.Contains(issueLine, "--days") {
				t.Errorf("a domain certificate was asked for as a short-lived one: %s", issueLine)
			}
			if !strings.Contains(lineWith(calls, "acme.sh --installcert"), "--reloadcmd my reload") {
				t.Errorf("the operator's reload command was not kept:\n%s", calls)
			}
		})
	}
}

// TestACMEIssueWebrootStopsWhenNginxCannotTakePort80: without nginx answering
// on port 80 the CA's request would go nowhere. Asking anyway spends one of
// the CA's failed-validation allowances for nothing.
func TestACMEIssueWebrootStopsWhenNginxCannotTakePort80(t *testing.T) {
	env, _, _, logFile := acmeStubs(t, true)
	out, err := runScriptShell(t, "install.sh", issueFunctions,
		"acme_issue_webroot '"+t.TempDir()+"' 3 '' 203.0.113.5\n", env...)
	if err == nil {
		t.Fatalf("the issuance reported success without nginx on port 80:\n%s", out)
	}
	if strings.Contains(readLog(t, logFile), "--issue") {
		t.Error("acme.sh was asked to issue although nginx could not take port 80")
	}
}

// A standalone-mode acme.sh domain config as acme.sh 3.x writes it.
func standaloneConf(name, reloadCmd string) string {
	return "Le_Domain='" + name + "'\nLe_Alt='no'\nLe_Webroot='no'\nLe_PreHook=''\nLe_PostHook=''\nLe_RenewHook=''\n" +
		"Le_API='https://acme-v02.api.letsencrypt.org/directory'\nLe_Keylength='ec-256'\nLe_Listen_V4='1'\n" +
		"Le_Certificate_Profile='shortlived'\n" +
		"Le_ReloadCmd='__ACME_BASE64__START_" + base64.StdEncoding.EncodeToString([]byte(reloadCmd)) + "__ACME_BASE64__END_'\n" +
		"Le_RealKeyPath='/root/cert/ip/privkey.pem'\nLe_CertCreateTime='1758800000'\n"
}

func writeConf(t *testing.T, home, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(home, ".acme.sh", dir, name+".conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var reloadRe = regexp.MustCompile(`(?m)^Le_ReloadCmd='__ACME_BASE64__START_(.*)__ACME_BASE64__END_'$`)

func decodedReload(t *testing.T, conf string) string {
	t.Helper()
	m := reloadRe.FindStringSubmatch(conf)
	if m == nil {
		t.Fatalf("no base64 Le_ReloadCmd in:\n%s", conf)
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("Le_ReloadCmd is not base64: %v", err)
	}
	return string(b)
}

// TestACMEMigrateSwitchesStandaloneConfigsToTheWebroot: a box installed before
// #138 has its certificates on acme.sh's renewal list in standalone mode. Once
// nginx holds port 80 those renewals would fail, so the update switches them
// to the webroot — in the config acme.sh reads on --renew, without asking the
// CA for anything — and makes their reload command reload nginx too.
func TestACMEMigrateSwitchesStandaloneConfigsToTheWebroot(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not available")
	}
	for _, script := range []string{"install.sh", "update.sh"} {
		t.Run(script, func(t *testing.T) {
			env, home, _, _ := acmeStubs(t, false)
			const oldReload = "systemctl restart x-ui 2>/dev/null || rc-service x-ui restart 2>/dev/null || true"
			ipConf := writeConf(t, home, "203.0.113.5_ecc", "203.0.113.5", standaloneConf("203.0.113.5", oldReload))
			rsaConf := writeConf(t, home, "vpn.example.com", "vpn.example.com", standaloneConf("vpn.example.com", "x-ui restart"))
			webroot := strings.Replace(standaloneConf("site.example.com", "systemctl reload apache2"), "Le_Webroot='no'", "Le_Webroot='/var/www/html'", 1)
			webrootConf := writeConf(t, home, "site.example.com_ecc", "site.example.com", webroot)
			dns := strings.Replace(standaloneConf("dns.example.com", "true"), "Le_Webroot='no'", "Le_Webroot='dns_cf'", 1)
			dnsConf := writeConf(t, home, "dns.example.com_ecc", "dns.example.com", dns)

			migrate := func() string {
				out, err := runScriptShell(t, script, []string{"acme_webroot", "acme_nginx_reload_cmd", "acme_migrate_to_webroot"},
					"acme_migrate_to_webroot\n", env...)
				if err != nil {
					t.Fatalf("acme_migrate_to_webroot: %v\n%s", err, out)
				}
				return out
			}
			migrate()

			ip := read(t, ipConf)
			if !strings.Contains(ip, "\nLe_Webroot='"+nginx.ACMEWebroot+"'\n") || strings.Contains(ip, "Le_Webroot='no'") {
				t.Errorf("the IP certificate still renews standalone:\n%s", ip)
			}
			reload := decodedReload(t, ip)
			if !strings.Contains(reload, "reload nginx") || !strings.Contains(reload, oldReload) {
				t.Errorf("reload command = %q, want nginx reloaded and the old command kept", reload)
			}
			for _, keep := range []string{"Le_Certificate_Profile='shortlived'", "Le_CertCreateTime='1758800000'", "Le_Domain='203.0.113.5'"} {
				if !strings.Contains(ip, keep) {
					t.Errorf("the migration lost %s", keep)
				}
			}
			if rsa := read(t, rsaConf); !strings.Contains(rsa, "Le_Webroot='"+nginx.ACMEWebroot+"'") {
				t.Errorf("an RSA (non-_ecc) standalone config was not switched:\n%s", rsa)
			}
			if got := read(t, webrootConf); got != webroot {
				t.Errorf("a certificate on somebody else's webroot was touched:\n%s", got)
			}
			if got := read(t, dnsConf); got != dns {
				t.Errorf("a DNS-validated certificate was touched:\n%s", got)
			}

			// Idempotent: a second update changes nothing.
			migrate()
			if again := read(t, ipConf); again != ip {
				t.Errorf("a second run changed the config again:\n%s\n---\n%s", ip, again)
			}
		})
	}
}

// TestNoStandaloneIssuanceLeft: acme.sh standalone is gone from every script;
// port 80 is nginx's.
func TestNoStandaloneIssuanceLeft(t *testing.T) {
	for _, script := range certScripts {
		body := read(t, script)
		for _, banned := range []string{"--standalone", "--httpport"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s still uses %s", script, banned)
			}
		}
	}
}

// TestCertHelpersAreTheSameInEveryScript: one issuing path for the panel and
// the hops means one text, copied into each script that has to run on its
// own. A fix made in one copy and not the others is exactly the drift that
// let the scripts disagree before.
func TestCertHelpersAreTheSameInEveryScript(t *testing.T) {
	shared := map[string][]string{
		"acme_webroot": certScripts, "acme_nginx_reload_cmd": certScripts, "acme_reload_cmd": certScripts,
		"acme_front_ready": certScripts, "acme_issue_webroot": certScripts, "acme_ip_flags": certScripts,
		"acme_migrate_to_webroot": {"install.sh", "update.sh"}, "acme_front_setup": {"install.sh", "update.sh"},
	}
	for name, scripts := range shared {
		first := scriptFunctions(t, scripts[0], name)
		if first == "" {
			t.Errorf("%s does not define %s", scripts[0], name)
			continue
		}
		for _, other := range scripts[1:] {
			if got := scriptFunctions(t, other, name); got != first {
				t.Errorf("%s in %s differs from %s:\n%s\n---\n%s", name, other, scripts[0], got, first)
			}
		}
	}
}

// TestScriptWebrootIsTheGoWebroot: the scripts tell acme.sh where to write,
// the binary tells nginx where to read. Two copies of one path.
func TestScriptWebrootIsTheGoWebroot(t *testing.T) {
	out, err := runScriptShell(t, "install.sh", []string{"acme_webroot"}, "acme_webroot\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != nginx.ACMEWebroot {
		t.Errorf("the scripts write challenges to %q, nginx serves %q", got, nginx.ACMEWebroot)
	}
}

func TestCertScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	for _, script := range certScripts {
		if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", script, err, out)
		}
	}
}

func lineWith(text, prefix string) string {
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestACMEFrontSetupLeavesRenewalsAloneWhenNginxCannotTakePort80: existing
// installs must not break. If nginx cannot serve port 80, switching acme.sh
// to the webroot would kill renewals that still work standalone.
func TestACMEFrontSetupLeavesRenewalsAloneWhenNginxCannotTakePort80(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not available")
	}
	functions := []string{"acme_webroot", "acme_nginx_reload_cmd", "acme_migrate_to_webroot", "acme_front_setup"}
	for _, script := range []string{"install.sh", "update.sh"} {
		t.Run(script, func(t *testing.T) {
			env, home, _, _ := acmeStubs(t, true)
			conf := standaloneConf("203.0.113.5", "x-ui restart")
			path := writeConf(t, home, "203.0.113.5_ecc", "203.0.113.5", conf)
			out, err := runScriptShell(t, script, functions, "acme_front_setup\n", env...)
			if err != nil {
				t.Fatalf("acme_front_setup is never fatal, got %v:\n%s", err, out)
			}
			if got := read(t, path); got != conf {
				t.Errorf("a standalone config was switched although nginx does not serve port 80:\n%s", got)
			}

			env, home, _, _ = acmeStubs(t, false)
			path = writeConf(t, home, "203.0.113.5_ecc", "203.0.113.5", conf)
			if out, err := runScriptShell(t, script, functions, "acme_front_setup\n", env...); err != nil {
				t.Fatalf("acme_front_setup: %v\n%s", err, out)
			}
			if got := read(t, path); !strings.Contains(got, "Le_Webroot='"+nginx.ACMEWebroot+"'") {
				t.Errorf("with nginx on port 80 the standalone config was not switched:\n%s", got)
			}
		})
	}
}
