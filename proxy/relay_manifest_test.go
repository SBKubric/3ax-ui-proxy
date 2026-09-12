package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/relaymanifest"
)

// TestRelayReadsABuiltManifest pins the contract between the panel's export
// and the relay: a manifest built from a raw config yields the same relayed
// ports the raw config would (public inbounds only; api tunnel and TPROXY
// inbounds skipped).
func TestRelayReadsABuiltManifest(t *testing.T) {
	raw := `{"inbounds":[
	  {"listen":"127.0.0.1","port":62789,"protocol":"tunnel","tag":"api"},
	  {"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443","settings":{"clients":[{"id":"x"}]},
	   "streamSettings":{"security":"reality","realitySettings":{"privateKey":"SECRET"}}},
	  {"listen":"::","port":12345,"protocol":"dokodemo-door","tag":"awg-tproxy-in",
	   "settings":{"followRedirect":true},"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}`
	m, err := relaymanifest.Build([]byte(raw), "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := m.JSON()
	path := filepath.Join(t.TempDir(), "relay-manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, ports, err := BuildRelayConfig(path, "203.0.113.1", "0.0.0.0", []ExtraPort{{Port: 51820, Network: "udp"}})
	if err != nil {
		t.Fatalf("BuildRelayConfig over a manifest: %v", err)
	}
	if len(ports) != 2 || ports[0] != 443 || ports[1] != 51820 {
		t.Fatalf("relayed ports = %v, want [443 51820]", ports)
	}
}
