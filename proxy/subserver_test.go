package proxy

import (
	"bytes"
	"encoding/base64"
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
		UpstreamHost: "1.2.3.4", XrayConfigPath: "x", UpstreamBase: "https://1.2.3.4:2096",
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
