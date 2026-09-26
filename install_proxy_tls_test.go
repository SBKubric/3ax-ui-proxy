package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// install.sh runs its whole install at the top level, so it cannot be sourced
// by a test. These tests lift the proxy-mode functions they exercise out of it
// by name and run them in a bash of their own, with nothing on the machine
// touched.

// scriptFunctions returns the named top-level functions of a script —
// install.sh, update.sh or x-ui.sh, which share their certificate helpers word
// for word. A name the script does not define is left out, so a test run
// against an older script fails on its assertions rather than on the
// extraction.
func scriptFunctions(t *testing.T, script string, names ...string) string {
	t.Helper()
	file, err := os.Open(script)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var out strings.Builder
	inside := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !inside {
			if name, found := strings.CutSuffix(line, "() {"); found && wanted[name] {
				inside = true
			}
		}
		if inside {
			out.WriteString(line + "\n")
			if line == "}" {
				inside = false
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// runInstallShell runs body after the given install.sh functions, with the
// installer's colour variables empty and env on top of a minimal environment.
func runInstallShell(t *testing.T, functions []string, body string, env ...string) (string, error) {
	t.Helper()
	return runScriptShell(t, "install.sh", functions, body, env...)
}

// runScriptShell is runInstallShell for any of the three scripts.
func runScriptShell(t *testing.T, scriptFile string, functions []string, body string, env ...string) (string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := "red='' green='' yellow='' blue='' plain=''\n" + scriptFunctions(t, scriptFile, functions...) + body
	cmd := exec.Command(bash, "-c", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeCert writes a self-signed certificate for ip, valid until notAfter, and
// its key, as the PEM files a hop serves its sub port with.
func writeCert(t *testing.T, dir, ip string, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		IPAddresses:  []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

var promptProxyModeFunctions = []string{
	"prompt_proxy_mode", "proxy_validate_config_values", "proxy_json_value_ok",
	"proxy_json_port_ok", "proxy_check_manual_tls",
}

func proxyModeEnv(tls, cert, key string) []string {
	return []string{
		"XUI_PROXY_MODE=1", "PROXY_NEXT_HOP=10.0.0.7", "PROXY_JOIN_TOKEN=0123456789012345678901234567890a",
		"PROXY_DOMAIN=10.0.0.9", "PROXY_TLS=" + tls, "PROXY_CERT=" + cert, "PROXY_KEY=" + key,
	}
}

// TestInstallProxyManualTLSRefusesACertItCannotServe (#124): with
// PROXY_TLS=manual a missing, empty or mismatched certificate used to cost the
// box its TLS without a word beyond a log line — the sub port came up on plain
// HTTP and the next-outer hop, polling it over https, only saw it unreachable.
// Now the installer stops before it has changed anything.
func TestInstallProxyManualTLSRefusesACertItCannotServe(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, filepath.Join(dir, "good"), "10.0.0.9", time.Now().Add(6*24*time.Hour))
	otherCert, _ := writeCert(t, filepath.Join(dir, "other"), "10.0.0.9", time.Now().Add(6*24*time.Hour))
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, cert, key string
		needsOpenSSL    bool
	}{
		{name: "no paths at all", cert: "", key: ""},
		{name: "no key", cert: cert, key: ""},
		{name: "a certificate that is not there", cert: filepath.Join(dir, "gone", "fullchain.pem"), key: key},
		{name: "a key that is not there", cert: cert, key: filepath.Join(dir, "gone", "privkey.pem")},
		{name: "an empty certificate", cert: empty, key: key},
		{name: "a relative path", cert: "cert/fullchain.pem", key: key},
		{name: "a certificate given as the key", cert: cert, key: cert},
		{name: "a key of another certificate", cert: otherCert, key: key, needsOpenSSL: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsOpenSSL {
				if _, err := exec.LookPath("openssl"); err != nil {
					t.Skip("openssl is not available")
				}
			}
			out, err := runInstallShell(t, promptProxyModeFunctions,
				"prompt_proxy_mode\necho \"reached the install with PROXY_CERT=${PROXY_CERT}\"\n",
				proxyModeEnv("manual", tc.cert, tc.key)...)
			if err == nil {
				t.Fatalf("the install went on with a certificate it cannot serve:\n%s", out)
			}
			if strings.Contains(out, "reached the install") {
				t.Errorf("prompt_proxy_mode returned instead of stopping:\n%s", out)
			}
			if !strings.Contains(out, "Nothing has been changed") {
				t.Errorf("the refusal does not say the box is untouched:\n%s", out)
			}
		})
	}
}

// TestInstallProxyManualTLSKeepsAServableCert: the reinstall the orchestrator
// runs on a joined hop — PROXY_TLS=manual with the certificate already on the
// box — goes through with both paths intact for config_proxy_mode.
func TestInstallProxyManualTLSKeepsAServableCert(t *testing.T) {
	cert, key := writeCert(t, t.TempDir(), "10.0.0.9", time.Now().Add(6*24*time.Hour))
	out, err := runInstallShell(t, promptProxyModeFunctions,
		"prompt_proxy_mode\necho \"cert=${PROXY_CERT} key=${PROXY_KEY}\"\n",
		proxyModeEnv("manual", cert, key)...)
	if err != nil {
		t.Fatalf("a servable manual certificate was refused: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cert="+cert+" key="+key) {
		t.Errorf("the paths did not survive the prompt:\n%s", out)
	}

	// PROXY_TLS=none is still the way to ask for plain HTTP.
	out, err = runInstallShell(t, promptProxyModeFunctions, "prompt_proxy_mode\n", proxyModeEnv("none", "", "")...)
	if err != nil {
		t.Errorf("PROXY_TLS=none was refused: %v\n%s", err, out)
	}
}

// TestInstallProxyReusesTheIPCertificateOnAReinstall (#124): a reinstall with
// PROXY_TLS=letsencrypt-ip used to ask Let's Encrypt for a fresh certificate
// every time. A few reinstalls into the week the CA refuses (five duplicates
// per 168 h), and a refused issuance left the hop on plain HTTP. The
// certificate a previous install left behind is kept while it is valid for
// this address and acme.sh still renews it.
func TestInstallProxyReusesTheIPCertificateOnAReinstall(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not available")
	}
	const ip = "203.0.113.5"
	check := func(t *testing.T, home, certDir, ip string) bool {
		t.Helper()
		out, err := runInstallShell(t, []string{"proxy_ip_cert_reusable"},
			"if proxy_ip_cert_reusable '"+certDir+"' '"+ip+"'; then echo reusable; else echo issue; fi\n",
			"HOME="+home)
		if err != nil {
			t.Fatalf("proxy_ip_cert_reusable: %v\n%s", err, out)
		}
		return strings.TrimSpace(out) == "reusable"
	}
	renewedBy := func(t *testing.T, home, ip string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, ".acme.sh", ip+"_ecc"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("valid, for this address, renewed by acme.sh", func(t *testing.T) {
		home, certDir := t.TempDir(), filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, ip, time.Now().Add(5*24*time.Hour))
		renewedBy(t, home, ip)
		if !check(t, home, certDir, ip) {
			t.Error("a valid certificate for this box was not reused — the reinstall spends an issuance")
		}
	})
	t.Run("no certificate yet", func(t *testing.T) {
		home := t.TempDir()
		renewedBy(t, home, ip)
		if check(t, home, filepath.Join(t.TempDir(), "ip"), ip) {
			t.Error("a box without a certificate skipped the issuance")
		}
	})
	t.Run("issued for another address", func(t *testing.T) {
		home, certDir := t.TempDir(), filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, "203.0.113.55", time.Now().Add(5*24*time.Hour))
		renewedBy(t, home, ip)
		if check(t, home, certDir, ip) {
			t.Error("a certificate for 203.0.113.55 was taken for 203.0.113.5")
		}
	})
	t.Run("expires within a day", func(t *testing.T) {
		home, certDir := t.TempDir(), filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, ip, time.Now().Add(12*time.Hour))
		renewedBy(t, home, ip)
		if check(t, home, certDir, ip) {
			t.Error("a certificate about to expire was kept")
		}
	})
	t.Run("acme.sh no longer renews it", func(t *testing.T) {
		home, certDir := t.TempDir(), filepath.Join(t.TempDir(), "ip")
		writeCert(t, certDir, ip, time.Now().Add(5*24*time.Hour))
		if check(t, home, certDir, ip) {
			t.Error("a certificate nobody renews was kept — it would lapse within the week")
		}
	})
}
