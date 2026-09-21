package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProxyCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadConfigV2 is the normal case: a joined box reads its next hop, its
// secret and the defaults it never has to spell out.
func TestLoadConfigV2(t *testing.T) {
	path := writeProxyCfg(t, `{"version":2,
	  "nextHop":{"host":"10.0.0.7"},
	  "hopSecret":"s3cr3t","domain":"edge.example.com","cert":"c.pem","key":"k.pem"}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Bootstrap() {
		t.Error("a config with a hopSecret must not be in bootstrap mode")
	}
	if cfg.NextHop.Host != "10.0.0.7" {
		t.Errorf("nextHop.host = %q", cfg.NextHop.Host)
	}
	if cfg.NextHop.SubPort != DefaultSubPort || cfg.NextHop.SubScheme != DefaultSubScheme {
		t.Errorf("nextHop defaults = %d/%q", cfg.NextHop.SubPort, cfg.NextHop.SubScheme)
	}
	if cfg.SubPort != DefaultSubPort || cfg.RelayListen != DefaultRelayListen {
		t.Errorf("own defaults = %d/%q", cfg.SubPort, cfg.RelayListen)
	}
	if cfg.StateDir != DefaultStateDir {
		t.Errorf("stateDir = %q, want %q", cfg.StateDir, DefaultStateDir)
	}
	if got, want := cfg.DocumentPath(), filepath.Join(DefaultStateDir, "document.json"); got != want {
		t.Errorf("DocumentPath() = %q, want %q", got, want)
	}
	if got, want := cfg.NextHopBase(), "https://10.0.0.7:2096"; got != want {
		t.Errorf("NextHopBase() = %q, want %q", got, want)
	}
	if !cfg.TLS() {
		t.Error("cert+key must mean TLS")
	}
	if len(cfg.LegacyWarnings()) != 0 {
		t.Errorf("a clean v2 config warned: %v", cfg.LegacyWarnings())
	}
	if cfg.StaleMinutes != DefaultStaleMinutes {
		t.Errorf("staleMinutes = %d", cfg.StaleMinutes)
	}
}

// TestLoadConfigV2WithoutSecretIsBootstrap: a box the installer wrote but that
// has not joined yet is a normal state, not an error — the join page comes up.
func TestLoadConfigV2WithoutSecretIsBootstrap(t *testing.T) {
	path := writeProxyCfg(t, `{"version":2,"nextHop":{"host":"10.0.0.7","subPort":8443,"subScheme":"http"}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Bootstrap() {
		t.Fatal("a config without a hopSecret must be in bootstrap mode")
	}
	if got, want := cfg.NextHopBase(), "http://10.0.0.7:8443"; got != want {
		t.Errorf("NextHopBase() = %q, want %q", got, want)
	}
	if got := cfg.NextHopHint(); got != "10.0.0.7" {
		t.Errorf("NextHopHint() = %q, want the configured host", got)
	}
}

// TestLoadConfigV1IsWarnedAboutAndBootstraps: update.sh reaches a live box
// before its owner does, so a v1 proxy.json must never be fatal. Every v1 key
// gets one WARN naming its replacement, the values are dropped, and the box
// comes up in join mode with upstreamHost only as a prefill hint.
func TestLoadConfigV1IsWarnedAboutAndBootstraps(t *testing.T) {
	path := writeProxyCfg(t, `{"upstreamHost":"203.0.113.1","relayManifestPath":"/etc/x-ui/relay-manifest.json",
	  "extraPorts":["51820/udp"],"upstreamBase":"https://203.0.113.1:2096","subPath":"/sub/","jsonPath":"/json/",
	  "subPort":2097,"domain":"old.example.com","relayListen":"0.0.0.0"}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a v1 config must load, got %v", err)
	}
	if !cfg.Bootstrap() {
		t.Error("a v1 config must land in bootstrap mode")
	}
	if cfg.NextHop.Host != "" {
		t.Errorf("upstreamHost must not become nextHop.host, got %q", cfg.NextHop.Host)
	}
	if got := cfg.NextHopHint(); got != "203.0.113.1" {
		t.Errorf("NextHopHint() = %q, want the legacy upstreamHost as a prefill", got)
	}
	// Keys shared by v1 and v2 are read as they are.
	if cfg.SubPort != 2097 || cfg.Domain != "old.example.com" || cfg.RelayListen != "0.0.0.0" {
		t.Errorf("shared keys were dropped: %+v", cfg)
	}
	warnings := strings.Join(cfg.LegacyWarnings(), "\n")
	for _, key := range []string{"upstreamHost", "relayManifestPath", "extraPorts", "upstreamBase", "subPath", "jsonPath"} {
		if !strings.Contains(warnings, `"`+key+`"`) {
			t.Errorf("no warning for the v1 key %q; got:\n%s", key, warnings)
		}
	}
	if n := len(cfg.LegacyWarnings()); n != 6 {
		t.Errorf("warning count = %d, want one per v1 key", n)
	}
	for _, want := range []string{"nextHop.host", "chain document"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("warnings do not name the replacement %q:\n%s", want, warnings)
		}
	}
}

// TestLoadConfigRefusesANewerVersion: half-applying a format this build does
// not know would be worse than refusing to start.
func TestLoadConfigRefusesANewerVersion(t *testing.T) {
	_, err := LoadConfig(writeProxyCfg(t, `{"version":3,"nextHop":{"host":"10.0.0.7"},"hopSecret":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "version 3") {
		t.Fatalf("want an error naming version 3, got %v", err)
	}
}

// TestSaveIsAtomicAndPrivate: the box writes its own hopSecret and nextHop
// here after a join — a truncated file would cost it the chain.
func TestSaveWritesAJoinablePrivateConfig(t *testing.T) {
	path := writeProxyCfg(t, `{"version":2,"nextHop":{"host":"10.0.0.7"},"subPort":2096}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.HopSecret = "the-hop-secret"
	cfg.NextHop.Host = "10.0.0.8"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", st.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Save left temporary files behind: %v", entries)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("saved config is not JSON: %v", err)
	}
	if raw["version"] != float64(ConfigVersion) {
		t.Errorf("saved version = %v, want %d", raw["version"], ConfigVersion)
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if back.HopSecret != "the-hop-secret" || back.NextHop.Host != "10.0.0.8" || back.Bootstrap() {
		t.Errorf("reloaded config = %+v", back)
	}
}

// TestPublicHostPortOmitsOnlyTheSchemeDefault: every place that builds this
// hop's own public URL from Domain must keep a non-default sub port, or
// clients end up on whatever else answers the scheme's default port — xray,
// on a box that terminates the sub port elsewhere (#98).
func TestPublicHostPortOmitsOnlyTheSchemeDefault(t *testing.T) {
	cases := []struct {
		name   string
		scheme string
		host   string
		port   int
		want   string
	}{
		{"https non-default port", "https", "proxy.example.com", 2096, "proxy.example.com:2096"},
		{"https default port", "https", "proxy.example.com", 443, "proxy.example.com"},
		{"http non-default port", "http", "proxy.example.com", 2096, "proxy.example.com:2096"},
		{"http default port", "http", "proxy.example.com", 80, "proxy.example.com"},
		{"https default port does not match http", "https", "proxy.example.com", 80, "proxy.example.com:80"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PublicHostPort(tc.scheme, tc.host, tc.port); got != tc.want {
				t.Errorf("PublicHostPort(%q, %q, %d) = %q, want %q", tc.scheme, tc.host, tc.port, got, tc.want)
			}
		})
	}
}
