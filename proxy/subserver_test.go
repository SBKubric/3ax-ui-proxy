package proxy

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/gin-gonic/gin"
)

// testSubServer builds a sub server over a state the test controls.
func testSubServer(t *testing.T, cfg *Config, state *State) *SubServer {
	t.Helper()
	if cfg.SubPort == 0 {
		cfg.SubPort = DefaultSubPort
	}
	if cfg.NextHop.SubPort == 0 {
		cfg.NextHop.SubPort = DefaultSubPort
	}
	if cfg.NextHop.SubScheme == "" {
		cfg.NextHop.SubScheme = DefaultSubScheme
	}
	s, err := NewSubServer(cfg, state, nil, nil)
	if err != nil {
		t.Fatalf("NewSubServer: %v", err)
	}
	return s
}

func TestDecodeConfigs(t *testing.T) {
	links := "vless://uuid@host:443?type=tcp#a\ntrojan://pw@host:8443#b"

	// base64-encoded body (how the panel returns it with subEncrypt on)
	got := decodeConfigs([]byte(base64.StdEncoding.EncodeToString([]byte(links))))
	if len(got) != 2 || got[0] != "vless://uuid@host:443?type=tcp#a" {
		t.Fatalf("base64 decode = %#v", got)
	}

	// plain newline list (subEncrypt off) — must not be mangled
	got = decodeConfigs([]byte(links + "\n"))
	if len(got) != 2 || got[1] != "trojan://pw@host:8443#b" {
		t.Fatalf("plain decode = %#v", got)
	}
}

func TestParseUserinfo(t *testing.T) {
	used, total, expire := parseUserinfo("upload=1048576; download=1048576; total=10485760; expire=0")
	if used == "" || total == "" {
		t.Fatalf("used=%q total=%q", used, total)
	}
	if expire != "" {
		t.Errorf("expire for 0 should be empty, got %q", expire)
	}

	_, total, expire = parseUserinfo("total=0; expire=1893456000")
	if total != "∞" {
		t.Errorf("total = %q, want ∞ for unlimited", total)
	}
	if expire == "" {
		t.Error("expire should be set for a non-zero timestamp")
	}

	if u, to, e := parseUserinfo(""); u != "" || to != "" || e != "" {
		t.Errorf("empty header should yield empties, got %q %q %q", u, to, e)
	}
}

func TestPageRenders(t *testing.T) {
	s := testSubServer(t, &Config{NextHop: NextHop{Host: "1.2.3.4"}}, NewState())

	var buf bytes.Buffer
	err := s.tmpl.Execute(&buf, pageData{
		Title: "Subscription", SubURL: "https://proxy/sub/abc", JsonURL: "https://proxy/json/abc",
		Configs: []string{"vless://a@h:443#x"}, Used: "1 MB", Total: "∞", Apps: recommendedApps,
	})
	if err != nil {
		t.Fatalf("template execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"https://proxy/sub/abc", "Copy VLESS JSON", "Amnezia", "DefaultVPN", "vless://a@h:443#x"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

// The panel stamps Profile-Web-Page-Url with the host it was fetched by — an
// address deeper in the chain — so raw subscriptions relayed by a hop must
// carry that hop's own address instead, or apps would link inward.
func TestPublicURLAndProfileHeaderRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &Config{NextHop: NextHop{Host: "1.2.3.4"}}
	s := testSubServer(t, cfg, NewState())

	newCtx := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/sub/abc", nil)
		c.Request.Host = "5.6.7.8:2096"
		return c, w
	}

	c, _ := newCtx()
	if got := s.publicURL(c, s.publicSubPath(), "abc"); got != "http://5.6.7.8:2096/sub/abc" {
		t.Errorf("publicURL without domain = %q", got)
	}
	// A non-default sub port must survive into the domain-based URL, or
	// clients built from it land on whatever else answers 443 — xray, on a
	// box that terminates the sub port elsewhere (#98).
	cfg.Domain = "proxy.example.com"
	cfg.CertFile, cfg.KeyFile = "c", "k"
	if got := s.publicURL(c, s.publicJsonPath(), "abc"); got != "https://proxy.example.com:2096/json/abc" {
		t.Errorf("publicURL with domain+TLS+non-default port = %q", got)
	}

	// The scheme's default port is left off: it need not appear for the URL
	// to reach the sub server.
	cfg.SubPort = 443
	if got := s.publicURL(c, s.publicJsonPath(), "abc"); got != "https://proxy.example.com/json/abc" {
		t.Errorf("publicURL with domain+TLS+default port = %q", got)
	}
	cfg.Domain, cfg.CertFile, cfg.KeyFile = "", "", ""
	cfg.SubPort = DefaultSubPort

	// The header copied from the panel names a hop deeper in; after
	// copyHeaders + rewrite the client must see this hop instead.
	c, w := newCtx()
	upstream := http.Header{}
	upstream.Set("Profile-Web-Page-Url", "https://1.2.3.4:2096/sub/abc")
	upstream.Set("Subscription-Userinfo", "upload=0; download=0; total=0; expire=0")
	copyHeaders(c, upstream)
	c.Header("Profile-Web-Page-Url", s.publicURL(c, s.publicSubPath(), "abc"))
	c.String(http.StatusOK, "ok")
	if got := w.Header().Get("Profile-Web-Page-Url"); got != "http://5.6.7.8:2096/sub/abc" {
		t.Errorf("Profile-Web-Page-Url = %q, want this hop's own URL", got)
	}
	if got := w.Header().Get("Subscription-Userinfo"); got == "" {
		t.Error("Subscription-Userinfo was not passed through")
	}
}

// TestSubscriptionsAre503BeforeTheFirstDocument: a hop with no document knows
// neither its upstream nor its paths. 503 says "not ready"; a 404 would look
// like a wrong link and send the owner hunting for a typo (§5.4).
func TestSubscriptionsAre503BeforeTheFirstDocument(t *testing.T) {
	s := testSubServer(t, &Config{NextHop: NextHop{Host: "1.2.3.4"}}, NewState())

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sub/abc", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/sub before a document: status %d, want 503", w.Code)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nothing-here", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("an unrelated path: status %d, want 404", w.Code)
	}
}

// TestSubscriptionUpstreamComesFromTheDocument: the next hop's address and the
// subscription paths travel in the document, so a revision that moves either
// takes effect without a restart and without a key in proxy.json (§5.2).
func TestSubscriptionUpstreamComesFromTheDocument(t *testing.T) {
	var asked []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Profile-Web-Page-Url", "https://the-real-server:2096/s/abc")
		w.Header().Set("Subscription-Userinfo", "upload=1; download=1; total=0; expire=0")
		_, _ = w.Write([]byte("vless://a@h:443#x"))
	}))
	defer upstream.Close()

	host, port := hostPort(t, upstream.URL)
	state := NewState()
	state.SetDocument(&chain.Document{
		Version:  chain.DocumentVersion,
		Revision: 42,
		Self:     chain.Self{Name: "edge-a", Role: chain.RoleEdge, Host: "edge.example.com"},
		NextHop: chain.NextHop{
			Host: host, SubPort: port, SubScheme: "http",
			SubPath: "/s/", JsonPath: "/j/",
		},
		Ports: []chain.Port{{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray}},
	})
	s := testSubServer(t, &Config{NextHop: NextHop{Host: "10.0.0.7"}, Domain: "edge.example.com"}, state)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/s/abc", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vless://") {
		t.Fatalf("subscription: status %d body %q", w.Code, w.Body.String())
	}
	if len(asked) != 1 || asked[0] != "/s/abc" {
		t.Fatalf("upstream was asked for %v, want the document's subPath", asked)
	}
	// cfg.SubPort defaults to 2096 here — not http's default 80 — so it must
	// appear in the domain-based URL (#98).
	if got, want := w.Header().Get("Profile-Web-Page-Url"), "http://edge.example.com:2096/s/abc"; got != want {
		t.Errorf("Profile-Web-Page-Url = %q, want %q", got, want)
	}

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/j/abc", nil))
	if w.Code != http.StatusOK {
		t.Errorf("json subscription: status %d", w.Code)
	}
	if len(asked) != 2 || asked[1] != "/j/abc" {
		t.Errorf("upstream was asked for %v, want the document's jsonPath", asked)
	}
}

// hostPort splits an httptest server URL into its host and port.
func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	trimmed := strings.TrimPrefix(rawURL, "http://")
	host, port, found := strings.Cut(trimmed, ":")
	if !found {
		t.Fatalf("unexpected test server URL %q", rawURL)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("unexpected test server port in %q: %v", rawURL, err)
	}
	return host, number
}

// TestTheUpdateIntervalReachesClientsThroughTheFront: while inbounds follow
// the chain the panel shortens Profile-Update-Interval so clients pick up the
// new SNI after a switch (#139). A front that dropped the header would leave
// its clients on the app's own default, so both the raw and the JSON
// subscription must carry it on unchanged.
func TestTheUpdateIntervalReachesClientsThroughTheFront(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Profile-Update-Interval", "1")
		_, _ = w.Write([]byte("vless://a@h:443#x"))
	}))
	defer upstream.Close()

	host, port := hostPort(t, upstream.URL)
	state := NewState()
	document := testDocument(42)
	document.NextHop = chain.NextHop{Host: host, SubPort: port, SubScheme: "http", SubPath: "/s/", JsonPath: "/j/"}
	state.SetDocument(document)
	s := testSubServer(t, &Config{NextHop: NextHop{Host: "10.0.0.7"}}, state)

	for _, path := range []string{"/s/abc", "/j/abc"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if got := w.Header().Get("Profile-Update-Interval"); got != "1" {
			t.Errorf("%s: Profile-Update-Interval = %q, want the panel's 1", path, got)
		}
	}
}
