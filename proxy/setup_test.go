package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

const testManifest = `[{"port":443,"network":"tcp,udp","tag":"inbound-443","source":"xray"}]`

func newTestSetup(t *testing.T) (*SetupServer, *Config) {
	t.Helper()
	cfg := &Config{
		UpstreamHost:      "1.2.3.4",
		RelayManifestPath: filepath.Join(t.TempDir(), "etc", "relay-manifest.json"),
		SubPort:           2096,
		Domain:            "proxy.example.com",
	}
	s, err := NewSetupServer(cfg)
	if err != nil {
		t.Fatalf("NewSetupServer: %v", err)
	}
	return s, cfg
}

func do(s *SetupServer, method, path, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestSetupPageIsOnlyAtTheSecretToken(t *testing.T) {
	s, _ := newTestSetup(t)
	if len(s.Token()) < 32 {
		t.Fatalf("token too short: %q", s.Token())
	}
	if w := do(s, http.MethodGet, "/setup/not-the-token", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("wrong token: status %d, want 404", w.Code)
	}
	if w := do(s, http.MethodGet, "/", "", ""); w.Code != http.StatusNotFound {
		t.Errorf("root: status %d, want 404", w.Code)
	}
	w := do(s, http.MethodGet, "/setup/"+s.Token(), "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<textarea") {
		t.Fatalf("setup page: status %d body %q", w.Code, w.Body.String())
	}
}

func TestSetupPageRefusesARawPanelConfig(t *testing.T) {
	s, cfg := newTestSetup(t)
	raw := `{"inbounds":[{"port":443,"protocol":"vless","streamSettings":{"realitySettings":{"privateKey":"SECRET"}}}]}`
	w := do(s, http.MethodPost, "/setup/"+s.Token(), "application/json", raw)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not a chain port list") {
		t.Fatalf("raw config: status %d body %q", w.Code, w.Body.String())
	}
	if _, err := os.Stat(cfg.RelayManifestPath); err == nil {
		t.Fatal("raw config was written to disk")
	}
	select {
	case <-s.Accepted():
		t.Fatal("raw config was accepted")
	default:
	}
	// The page stays up for another try.
	if w := do(s, http.MethodGet, "/setup/"+s.Token(), "", ""); w.Code != http.StatusOK {
		t.Fatalf("page gone after a bad paste: %d", w.Code)
	}
}

func TestSetupPageAcceptsAPortListOnceAndGoesDark(t *testing.T) {
	s, cfg := newTestSetup(t)
	form := url.Values{"manifest": {testManifest}}.Encode()
	w := do(s, http.MethodPost, "/setup/"+s.Token(), "application/x-www-form-urlencoded", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "443") {
		t.Fatalf("valid port list: status %d body %q", w.Code, w.Body.String())
	}
	select {
	case <-s.Accepted():
	default:
		t.Fatal("Accepted() not signalled")
	}
	data, err := os.ReadFile(cfg.RelayManifestPath)
	if err != nil {
		t.Fatalf("port list not written: %v", err)
	}
	ports, err := ParseRelayPorts(data)
	if err != nil {
		t.Fatalf("written file is not a valid port list: %v", err)
	}
	if len(ports) != 1 || ports[0].Port != 443 || ports[0].Network != chain.NetworkTCPUDP {
		t.Fatalf("written port list = %+v, want the pasted one", ports)
	}
	if st, _ := os.Stat(cfg.RelayManifestPath); st.Mode().Perm() != 0o600 {
		t.Errorf("port list mode = %o, want 600", st.Mode().Perm())
	}
	// One-shot: the token is spent.
	if w := do(s, http.MethodGet, "/setup/"+s.Token(), "", ""); w.Code != http.StatusNotFound {
		t.Errorf("page still up after acceptance: %d", w.Code)
	}
	if w := do(s, http.MethodPost, "/setup/"+s.Token(), "application/json", testManifest); w.Code != http.StatusNotFound {
		t.Errorf("second paste accepted: %d", w.Code)
	}
}

func TestSetupURLFollowsTheSubscriptionEndpoint(t *testing.T) {
	s, cfg := newTestSetup(t)
	if got, want := s.URL(), "http://proxy.example.com:2096/setup/"+s.Token(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
	cfg.CertFile, cfg.KeyFile = "c.pem", "k.pem"
	if got := s.URL(); !strings.HasPrefix(got, "https://proxy.example.com:2096/") {
		t.Errorf("URL() with TLS = %q", got)
	}
}
