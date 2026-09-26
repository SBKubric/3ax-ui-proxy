package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

// useIPCertDir points the panel at a temporary /root/cert/ip.
func useIPCertDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ip")
	prev := ipCertDir
	ipCertDir = dir
	t.Cleanup(func() { ipCertDir = prev })
	return dir
}

// useCertDirs points the directory search at dirs.
func useCertDirs(t *testing.T, dirs ...string) {
	t.Helper()
	prev := certDirs
	certDirs = dirs
	t.Cleanup(func() { certDirs = prev })
}

// writeIPCert writes the IP certificate the way install.sh installs it:
// fullchain.pem and privkey.pem in dir, for the given SANs.
func writeIPCert(t *testing.T, dir string, notAfter time.Time, ips []string, names []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     notAfter,
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIPCertificateIsFoundWhereInstallShPutsIt(t *testing.T) {
	dir := useIPCertDir(t)

	if _, _, _, err := ipCertificate(); !errors.Is(err, errNoCertificate) {
		t.Errorf("a box without an IP certificate: err = %v, want errNoCertificate", err)
	}

	expires := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	writeIPCert(t, dir, expires, []string{"203.0.113.5"}, nil)
	cert, key, expiry, err := ipCertificate()
	if err != nil {
		t.Fatalf("a valid IP certificate was refused: %v", err)
	}
	if cert != filepath.Join(dir, "fullchain.pem") || key != filepath.Join(dir, "privkey.pem") {
		t.Errorf("paths = %s, %s", cert, key)
	}
	if !expiry.Equal(expires.UTC()) && !expiry.Equal(expires) {
		t.Errorf("expiry = %v, want %v", expiry, expires)
	}
}

// TestIPCertificateMustBeForAnAddressAndValid: handing nginx an expired
// certificate, or one for a name, as the answer to requests by address would
// greet every client with a certificate error.
func TestIPCertificateMustBeForAnAddressAndValid(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		dir := useIPCertDir(t)
		writeIPCert(t, dir, time.Now().Add(-time.Hour), []string{"203.0.113.5"}, nil)
		if _, _, _, err := ipCertificate(); err == nil || errors.Is(err, errNoCertificate) {
			t.Errorf("an expired IP certificate: err = %v", err)
		}
	})
	t.Run("for a name, not an address", func(t *testing.T) {
		dir := useIPCertDir(t)
		writeIPCert(t, dir, time.Now().Add(5*24*time.Hour), nil, []string{"example.net"})
		if _, _, _, err := ipCertificate(); err == nil || errors.Is(err, errNoCertificate) {
			t.Errorf("a certificate without an IP SAN: err = %v", err)
		}
	})
}

// TestDomainCertificatePrefersThePanelSettings: the operator's domain is
// served with the certificate they gave the panel, not with whatever the
// directory search turns up first.
func TestDomainCertificatePrefersThePanelSettings(t *testing.T) {
	s := newNginxTestServer(t)
	settingsCert, settingsKey := writeTestCert(t, t.TempDir(), "vpn.example.com")
	dirCert, _ := writeTestCert(t, t.TempDir(), "vpn.example.com")
	useCertDirs(t, filepath.Dir(filepath.Dir(dirCert)))

	var setting SettingService
	// The sub server's certificate covers the domain; the panel's does not.
	otherCert, otherKey := writeTestCert(t, t.TempDir(), "other.example.com")
	for k, v := range map[string]string{
		"webCertFile": otherCert, "webKeyFile": otherKey,
		"subCertFile": settingsCert, "subKeyFile": settingsKey,
	} {
		if err := setting.setString(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cert, key, _, err := s.domainCertificate("vpn.example.com")
	if err != nil {
		t.Fatalf("domainCertificate: %v", err)
	}
	if cert != settingsCert || key != settingsKey {
		t.Errorf("got %s, want the sub server's certificate from the settings %s", cert, settingsCert)
	}

	// The panel's own certificate, once it covers the domain, comes first.
	if err := setting.setString("webCertFile", settingsCert); err != nil {
		t.Fatal(err)
	}
	if err := setting.setString("webKeyFile", settingsKey); err != nil {
		t.Fatal(err)
	}
	if cert, _, _, _ := s.domainCertificate("vpn.example.com"); cert != settingsCert {
		t.Errorf("got %s, want the panel's certificate", cert)
	}

	// Neither setting covers the domain: the directory search, as before.
	if cert, _, _, err := s.domainCertificate("vpn.example.com"); err != nil || cert == "" {
		t.Fatalf("fallback: %v", err)
	}
	for _, k := range []string{"webCertFile", "subCertFile"} {
		if err := setting.setString(k, otherCert); err != nil {
			t.Fatal(err)
		}
	}
	if cert, _, _, err := s.domainCertificate("vpn.example.com"); err != nil || cert != dirCert {
		t.Errorf("got %s (%v), want the one found in the certificate directories %s", cert, err, dirCert)
	}
}

// TestBuildConfigServesTheAddressWithTheIPCertificate: a box with an IP
// certificate and no domain still gets an HTTP side, answering by address.
func TestBuildConfigServesTheAddressWithTheIPCertificate(t *testing.T) {
	s := newNginxTestServer(t)
	seedInbounds(t)
	dir := useIPCertDir(t)
	useCertDirs(t, t.TempDir())
	writeIPCert(t, dir, time.Now().Add(5*24*time.Hour), []string{"203.0.113.5"}, nil)

	cfg, err := s.buildConfig(NginxSettings{Mode: string(nginx.ModeShared), RealityPort: 8443})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if cfg.Site == nil {
		t.Fatal("no HTTP side although the box has an IP certificate")
	}
	if cfg.Site.Domain != "" || cfg.Site.IPCertFile != filepath.Join(dir, "fullchain.pem") || cfg.Site.IPKeyFile != filepath.Join(dir, "privkey.pem") {
		t.Errorf("site = %+v", cfg.Site)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the config does not validate: %v", err)
	}

	// And with a domain: both, the domain from its own certificate.
	domainCert, _ := writeTestCert(t, t.TempDir(), "vpn.example.com")
	useCertDirs(t, filepath.Dir(filepath.Dir(domainCert)))
	cfg, err = s.buildConfig(NginxSettings{Mode: string(nginx.ModeShared), Domain: "vpn.example.com", RealityPort: 8443})
	if err != nil {
		t.Fatalf("buildConfig with a domain: %v", err)
	}
	if cfg.Site.CertFile != domainCert || cfg.Site.IPCertFile == "" {
		t.Errorf("site = %+v", cfg.Site)
	}
}

// TestBuildConfigWithoutAnyCertificateKeepsPassthrough: nothing to terminate
// TLS with, nothing on the HTTP side — as before.
func TestBuildConfigWithoutAnyCertificateKeepsPassthrough(t *testing.T) {
	s := newNginxTestServer(t)
	seedInbounds(t)
	useIPCertDir(t)
	cfg, err := s.buildConfig(NginxSettings{Mode: string(nginx.ModeShared), RealityPort: 8443})
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if cfg.Site != nil {
		t.Errorf("a site was built with nothing to serve it with: %+v", cfg.Site)
	}
}

// TestBrokenIPCertificateIsReported: a certificate that is there but cannot be
// served is worth a word on the Nginx page; one that is simply absent is not.
func TestBrokenIPCertificateIsReported(t *testing.T) {
	s := newNginxTestServer(t)
	dir := useIPCertDir(t)
	if hasWarning(s.GetStatus().Warnings, "certProblem") {
		t.Error("a box without an IP certificate was warned about it")
	}
	writeIPCert(t, dir, time.Now().Add(-time.Hour), []string{"203.0.113.5"}, nil)
	if !hasWarning(s.GetStatus().Warnings, "certProblem") {
		t.Errorf("an expired IP certificate went unreported: %v", s.GetStatus().Warnings)
	}
}

func hasWarning(warnings []NginxWarning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestBuildConfigRefusesABrokenIPCertificate: an IP certificate that is there
// but cannot be served is an error, as a broken domain certificate is — not a
// reason to quietly render the HTTP side away, which would have the reconcile
// reload nginx (and restart Xray) without the address side. An error keeps
// the running config and puts the reconcile into backoff.
func TestBuildConfigRefusesABrokenIPCertificate(t *testing.T) {
	s := newNginxTestServer(t)
	seedInbounds(t)
	dir := useIPCertDir(t)
	useCertDirs(t, t.TempDir())
	writeIPCert(t, dir, time.Now().Add(-time.Hour), []string{"203.0.113.5"}, nil)

	if _, err := s.buildConfig(NginxSettings{Mode: string(nginx.ModeShared), RealityPort: 8443}); err == nil {
		t.Fatal("an expired IP certificate was silently left out of the config")
	}
}
