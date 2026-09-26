package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
if [[ "$1" == "--issue" ]]; then
    # Like acme.sh, the first attempt leaves the domain's record behind even
    # when the order fails.
    mkdir -p "$HOME/.acme.sh/$3_ecc" && echo "Le_Domain='$3'" >"$HOME/.acme.sh/$3_ecc/$3.conf"
    if [[ -n "${STUB_ISSUE_FAIL:-}" ]]; then
        echo "${STUB_ISSUE_FAIL}"
        exit 1
    fi
fi
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
// reload command that reloads nginx as well as restarting x-ui. An address
// always gets the short-lived profile renewed every 3 days: the certificate
// lives ~160 h, so renewing at 6 days (the panel menus' old value) left ~16 h
// for a daily cron to hit.
func TestACMEIssueWebrootIssuesThroughNginx(t *testing.T) {
	for _, script := range certScripts {
		t.Run(script, func(t *testing.T) {
			env, _, _, logFile := acmeStubs(t, false)
			certDir := filepath.Join(t.TempDir(), "ip")

			out, err := runScriptShell(t, script, issueFunctions,
				"install_nginx() { echo install_nginx; }\nacme_issue_webroot '"+certDir+"' '' 203.0.113.5\n", env...)
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
// no short-lived profile, acme.sh's own renewal interval — and keeps a reload
// command the operator chose.
func TestACMEIssueWebrootForADomain(t *testing.T) {
	for _, script := range certScripts {
		t.Run(script, func(t *testing.T) {
			env, _, _, logFile := acmeStubs(t, false)
			certDir := filepath.Join(t.TempDir(), "vpn.example.com")
			out, err := runScriptShell(t, script, issueFunctions,
				"acme_issue_webroot '"+certDir+"' 'my reload' vpn.example.com\n", env...)
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
		"acme_issue_webroot '"+t.TempDir()+"' '' 203.0.113.5\n", env...)
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
		"hop_install_nginx": {"install.sh", "update.sh"},
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

func countLines(text, prefix string) int {
	n := 0
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestACMEIssueWebrootRetriesOnlyWhenTheCAIsUnreachable: the retry exists for
// a CA that did not answer (a cold dual-stack connect eating curl's timeout).
// A validation failure or a rate limit is an answer: asking again spends
// another of the CA's failed-validation allowances, and the operator has to
// hear that port 80 is the problem.
func TestACMEIssueWebrootRetriesOnlyWhenTheCAIsUnreachable(t *testing.T) {
	cases := []struct {
		name, output string
		issues       int
		says         string
	}{
		{"CA unreachable", "[Fri] Cannot init API for https://acme-v02.api.letsencrypt.org/directory", 2, "did not answer"},
		{"curl error", "[Fri] Please refer to https://curl.haxx.se/libcurl/c/libcurl-errors.html for error code: 28", 2, "did not answer"},
		{"validation failed", "[Fri] 203.0.113.5: Invalid status. Verification error details: 203.0.113.5: Fetching http://203.0.113.5/.well-known/acme-challenge/x: Connection refused", 1, "validation failed"},
		{"rate limited", `[Fri] Create new order error. Le_OrderFinalize not found. {"type":"urn:ietf:params:acme:error:rateLimited","detail":"too many certificates"}`, 1, "validation failed"},
		{"something else", "[Fri] Sign failed", 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, _, logFile := acmeStubs(t, false)
			env = append(env, "STUB_ISSUE_FAIL="+tc.output)
			out, err := runScriptShell(t, "install.sh", issueFunctions,
				"acme_issue_webroot '"+t.TempDir()+"/ip' '' 203.0.113.5\n", env...)
			if err == nil {
				t.Fatalf("a failed issuance reported success:\n%s", out)
			}
			if got := countLines(readLog(t, logFile), "acme.sh --issue"); got != tc.issues {
				t.Errorf("--issue ran %d times, want %d:\n%s", got, tc.issues, out)
			}
			if tc.says != "" && !strings.Contains(strings.ToLower(out), tc.says) {
				t.Errorf("the output does not say %q:\n%s", tc.says, out)
			}
		})
	}
}

// TestACMEIssueWebrootCleansUpOnlyWhatItCreated: a failed attempt used to
// delete the certificate directory and acme.sh's record wholesale — on a box
// with a working certificate, that is the working certificate and its
// renewals gone because a re-issue did not go through.
func TestACMEIssueWebrootCleansUpOnlyWhatItCreated(t *testing.T) {
	t.Run("a first attempt that fails leaves nothing behind", func(t *testing.T) {
		env, home, _, _ := acmeStubs(t, false)
		env = append(env, "STUB_ISSUE_FAIL=Invalid status")
		certDir := filepath.Join(t.TempDir(), "ip")
		if _, err := runScriptShell(t, "install.sh", issueFunctions, "acme_issue_webroot '"+certDir+"' '' 203.0.113.5\n", env...); err == nil {
			t.Fatal("expected a failure")
		}
		if _, err := os.Stat(filepath.Join(home, ".acme.sh", "203.0.113.5_ecc")); !os.IsNotExist(err) {
			t.Error("the record this attempt created was left behind")
		}
		if _, err := os.Stat(certDir); !os.IsNotExist(err) {
			t.Error("the directory this attempt created was left behind")
		}
	})
	t.Run("a re-issue that fails keeps the certificate that works", func(t *testing.T) {
		env, home, _, _ := acmeStubs(t, false)
		env = append(env, "STUB_ISSUE_FAIL=Invalid status")
		record := writeConf(t, home, "203.0.113.5_ecc", "203.0.113.5", standaloneConf("203.0.113.5", "x-ui restart"))
		certDir := filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, "203.0.113.5", time.Now().Add(4*24*time.Hour))
		if _, err := runScriptShell(t, "install.sh", issueFunctions, "acme_issue_webroot '"+certDir+"' '' 203.0.113.5\n", env...); err == nil {
			t.Fatal("expected a failure")
		}
		if _, err := os.Stat(record); err != nil {
			t.Error("acme.sh's record of the working certificate was deleted")
		}
		if _, err := os.Stat(filepath.Join(certDir, "fullchain.pem")); err != nil {
			t.Error("the working certificate was deleted")
		}
	})
	t.Run("nginx not on port 80 touches nothing", func(t *testing.T) {
		env, home, _, logFile := acmeStubs(t, true)
		certDir := filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, "203.0.113.5", time.Now().Add(4*24*time.Hour))
		if _, err := runScriptShell(t, "install.sh", issueFunctions, "acme_issue_webroot '"+certDir+"' '' 203.0.113.5\n", env...); err == nil {
			t.Fatal("expected a failure")
		}
		if strings.Contains(readLog(t, logFile), "--issue") {
			t.Error("acme.sh was asked although nginx could not take port 80")
		}
		if _, err := os.Stat(filepath.Join(certDir, "privkey.pem")); err != nil {
			t.Error("the certificate was deleted although the CA was never asked")
		}
		_ = home
	})
}

// TestIssuanceCallersDoNotDeleteCertificates: the callers used to clean up
// after a failure with rm -rf of the certificate directory and of acme.sh's
// record. acme_issue_webroot now removes exactly what the attempt created and
// nothing else, so no caller may do it on its own.
func TestIssuanceCallersDoNotDeleteCertificates(t *testing.T) {
	callers := map[string][]string{
		"install.sh": {"setup_ip_certificate", "ssl_cert_issue", "proxy_setup_tls"},
		"update.sh":  {"setup_ip_certificate", "ssl_cert_issue"},
		"x-ui.sh":    {"ssl_cert_issue", "ssl_cert_issue_for_ip"},
	}
	for script, names := range callers {
		for _, name := range names {
			body := scriptFunctions(t, script, name)
			if body == "" {
				t.Errorf("%s has no %s", script, name)
				continue
			}
			for _, bad := range []string{"rm -rf ~/.acme.sh", "rm -rf ${certDir}", "rm -rf \"$certPath\"", "rm -rf ${certPath}"} {
				if strings.Contains(body, bad) {
					t.Errorf("%s %s still runs %q", script, name, bad)
				}
			}
		}
	}
}

// TestACMEFrontFailureIsRememberedForTheRun: when acme_front_setup could not
// give port 80 to nginx — and stopped the nginx this run installed on a hop —
// the issuance later in the same run must not start it again through
// acme_front_ready; the box stays on the path it had.
func TestACMEFrontFailureIsRememberedForTheRun(t *testing.T) {
	env, _, _, logFile := acmeStubs(t, true)
	functions := append([]string{"acme_migrate_to_webroot", "acme_front_setup"}, issueFunctions...)
	out, err := runScriptShell(t, "install.sh", functions,
		"acme_front_setup\nif acme_issue_webroot '"+t.TempDir()+"' '' 203.0.113.5; then echo issued; fi\n", env...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	calls := readLog(t, logFile)
	if n := countLines(calls, "x-ui nginx acme-front"); n != 1 {
		t.Errorf("acme-front ran %d times in one run, want once:\n%s", n, calls)
	}
	if strings.Contains(calls, "--issue") || strings.Contains(out, "issued") {
		t.Errorf("an issuance went ahead after nginx could not take port 80:\n%s", out)
	}
}

// TestUninstallPutsTheStandaloneRenewalsBack: x-ui.sh uninstall removes the
// webroot nginx served. A certificate still pointing acme.sh at it would fail
// its next renewal, so uninstall reverses the migration.
func TestUninstallPutsTheStandaloneRenewalsBack(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not available")
	}
	env, home, _, _ := acmeStubs(t, false)
	const oldReload = "systemctl restart x-ui 2>/dev/null || true"
	migrated := writeConf(t, home, "203.0.113.5_ecc", "203.0.113.5", standaloneConf("203.0.113.5", oldReload))
	issued := writeConf(t, home, "vpn.example.com_ecc", "vpn.example.com", standaloneConf("vpn.example.com", "unused"))
	other := strings.Replace(standaloneConf("site.example.com", "systemctl reload apache2"), "Le_Webroot='no'", "Le_Webroot='/var/www/html'", 1)
	otherConf := writeConf(t, home, "site.example.com_ecc", "site.example.com", other)

	if out, err := runScriptShell(t, "install.sh", []string{"acme_webroot", "acme_nginx_reload_cmd", "acme_migrate_to_webroot"}, "acme_migrate_to_webroot\n", env...); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	// A certificate issued through the webroot from the start: its reload
	// command is acme_reload_cmd.
	fresh := strings.Replace(read(t, issued), "Le_Webroot='no'", "Le_Webroot='"+nginx.ACMEWebroot+"'", 1)
	fresh = reloadRe.ReplaceAllString(fresh, "Le_ReloadCmd='__ACME_BASE64__START_"+
		base64.StdEncoding.EncodeToString([]byte("systemctl reload nginx 2>/dev/null || nginx -s reload 2>/dev/null; systemctl restart x-ui 2>/dev/null || rc-service x-ui restart 2>/dev/null || true"))+"__ACME_BASE64__END_'")
	if err := os.WriteFile(issued, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}

	unmigrate := func() {
		out, err := runScriptShell(t, "x-ui.sh", []string{"acme_webroot", "acme_nginx_reload_cmd", "acme_unmigrate_from_webroot"}, "acme_unmigrate_from_webroot\n", env...)
		if err != nil {
			t.Fatalf("unmigrate: %v\n%s", err, out)
		}
	}
	unmigrate()

	back := read(t, migrated)
	if !strings.Contains(back, "\nLe_Webroot='no'\n") {
		t.Errorf("the certificate still renews through the removed webroot:\n%s", back)
	}
	if got := decodedReload(t, back); got != oldReload {
		t.Errorf("reload command = %q, want the original %q back", got, oldReload)
	}
	if got := decodedReload(t, read(t, issued)); got != "systemctl restart x-ui 2>/dev/null || rc-service x-ui restart 2>/dev/null || true" {
		t.Errorf("reload command of a webroot-issued certificate = %q, want the nginx reload gone", got)
	}
	if !strings.Contains(read(t, issued), "Le_Webroot='no'") {
		t.Error("a webroot-issued certificate was not put on standalone")
	}
	if got := read(t, otherConf); got != other {
		t.Errorf("a certificate on another webroot was touched:\n%s", got)
	}

	again := read(t, migrated)
	unmigrate()
	if read(t, migrated) != again {
		t.Error("a second run changed the config again")
	}
}

// hopShell is a PATH with the handful of tools hop_install_nginx needs and
// stub package managers, but no nginx: a hop before its first update.
func hopShell(t *testing.T) (env []string, logFile, policy string) {
	t.Helper()
	bin := t.TempDir()
	logFile = filepath.Join(t.TempDir(), "calls.log")
	policy = filepath.Join(t.TempDir(), "policy-rc.d")
	for _, tool := range []string{"rm", "chmod", "mkdir", "cat", "grep"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not available", tool)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	// The package manager notes whether a policy forbidding service starts is
	// in place while it runs — that is what keeps the postinst from starting
	// nginx with the distro's default site.
	aptGet := "#!/bin/bash\nif [[ -x '" + policy + "' ]] && grep -q 'exit 101' '" + policy + "'; then p=held; else p=open; fi\necho \"apt-get $* policy=$p\" >>'" + logFile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "apt-get"), []byte(aptGet), 0o755); err != nil {
		t.Fatal(err)
	}
	env = []string{"PATH=" + bin, "release=debian", "XUI_POLICY_RC_D=" + policy}
	return env, logFile, policy
}

// TestHopInstallNginxLeavesPort80ToWhoeverHasIt: a hop where something that
// is not nginx holds port 80 — acme.sh's own standalone renewal among the
// possibilities — keeps the standalone path; nginx is not installed there.
func TestHopInstallNginxLeavesPort80ToWhoeverHasIt(t *testing.T) {
	for _, script := range []string{"install.sh", "update.sh"} {
		t.Run(script, func(t *testing.T) {
			env, logFile, _ := hopShell(t)
			out, err := runScriptShell(t, script, []string{"hop_install_nginx"},
				"is_port_in_use() { return 0; }\nhop_install_nginx\necho \"installed_now=${nginx_installed_now:-0}\"\n", env...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if strings.Contains(readLog(t, logFile), "apt-get") {
				t.Error("nginx was installed although port 80 belongs to something else")
			}
			if !strings.Contains(out, "installed_now=0") || !strings.Contains(out, "port 80") {
				t.Errorf("no word about port 80, or the run believes it installed nginx:\n%s", out)
			}
		})
	}
}

// TestHopInstallNginxDoesNotLetThePackageStartIt: Debian's postinst starts
// nginx, with the default site on :80, the moment the package lands. On a hop
// that is the port acme.sh's standalone renewal needs until acme-front has
// taken over, so the start is held back while the package installs, and the
// hold is lifted afterwards.
func TestHopInstallNginxDoesNotLetThePackageStartIt(t *testing.T) {
	for _, script := range []string{"install.sh", "update.sh"} {
		t.Run(script, func(t *testing.T) {
			env, logFile, policy := hopShell(t)
			out, err := runScriptShell(t, script, []string{"hop_install_nginx"},
				"is_port_in_use() { return 1; }\nhop_install_nginx\necho \"installed_now=${nginx_installed_now:-0}\"\n", env...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			calls := readLog(t, logFile)
			if !strings.Contains(calls, "install") || !strings.Contains(calls, "policy=held") || strings.Contains(calls, "policy=open") {
				t.Errorf("the package was not installed under a no-start policy:\n%s", calls)
			}
			if _, err := os.Stat(policy); !os.IsNotExist(err) {
				t.Error("the no-start policy was left in place")
			}
			if !strings.Contains(out, "installed_now=1") {
				t.Errorf("the run does not remember it installed nginx:\n%s", out)
			}

			// A policy the operator had is theirs: used as is, never removed.
			if err := os.WriteFile(policy, []byte("#!/bin/sh\nexit 101\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := runScriptShell(t, script, []string{"hop_install_nginx"}, "is_port_in_use() { return 1; }\nhop_install_nginx\n", env...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if _, err := os.Stat(policy); err != nil {
				t.Error("the operator's own policy-rc.d was removed")
			}
		})
	}
}
