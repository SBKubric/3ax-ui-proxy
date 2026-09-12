package proxy

import (
	"net/http"
	"net/http/httptest"

	"bytes"
	"encoding/base64"
	"github.com/gin-gonic/gin"
	"strings"
	"testing"
)

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
	s, err := NewSubServer(&Config{
		UpstreamHost: "1.2.3.4", RelayManifestPath: "x", UpstreamBase: "https://1.2.3.4:2096",
		SubPath: "/sub/", JsonPath: "/json/", SubPort: 2096,
	})
	if err != nil {
		t.Fatalf("NewSubServer: %v", err)
	}

	var buf bytes.Buffer
	err = s.tmpl.Execute(&buf, pageData{
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

// The panel stamps Profile-Web-Page-Url with the host it was fetched by — the
// real server — so raw subscriptions relayed by the proxy must carry the
// proxy's own address instead, or apps would link straight to the hidden box.
func TestPublicURLAndProfileHeaderRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, err := NewSubServer(&Config{
		UpstreamHost: "1.2.3.4", RelayManifestPath: "x", UpstreamBase: "https://1.2.3.4:2096",
		SubPath: "/sub/", JsonPath: "/json/", SubPort: 2096,
	})
	if err != nil {
		t.Fatalf("NewSubServer: %v", err)
	}

	newCtx := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/sub/abc", nil)
		c.Request.Host = "5.6.7.8:2096"
		return c, w
	}

	c, _ := newCtx()
	if got := s.publicURL(c, s.cfg.SubPath, "abc"); got != "http://5.6.7.8:2096/sub/abc" {
		t.Errorf("publicURL without domain = %q", got)
	}
	s.cfg.Domain = "proxy.example.com"
	s.cfg.CertFile, s.cfg.KeyFile = "c", "k"
	if got := s.publicURL(c, s.cfg.JsonPath, "abc"); got != "https://proxy.example.com/json/abc" {
		t.Errorf("publicURL with domain+TLS = %q", got)
	}
	s.cfg.Domain, s.cfg.CertFile, s.cfg.KeyFile = "", "", ""

	// The header copied from the panel names the real server; after
	// copyHeaders + rewrite the client must see the proxy instead.
	c, w := newCtx()
	upstream := http.Header{}
	upstream.Set("Profile-Web-Page-Url", "https://1.2.3.4:2096/sub/abc")
	upstream.Set("Subscription-Userinfo", "upload=0; download=0; total=0; expire=0")
	copyHeaders(c, upstream)
	c.Header("Profile-Web-Page-Url", s.publicURL(c, s.cfg.SubPath, "abc"))
	c.String(http.StatusOK, "ok")
	if got := w.Header().Get("Profile-Web-Page-Url"); got != "http://5.6.7.8:2096/sub/abc" {
		t.Errorf("Profile-Web-Page-Url = %q, want the proxy's own URL", got)
	}
	if got := w.Header().Get("Subscription-Userinfo"); got == "" {
		t.Error("Subscription-Userinfo was not passed through")
	}
}
