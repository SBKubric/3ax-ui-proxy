package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildRelayConfig(t *testing.T) {
	// Shape mirrors a real panel config: an internal gRPC api tunnel on loopback,
	// two public inbounds (explicit 0.0.0.0 and implicit empty listen), and an
	// internal loopback inbound. Only the two public ports should be relayed.
	panelCfg := `{
		"inbounds": [
			{"listen":"127.0.0.1","port":62789,"protocol":"dokodemo-door","tag":"api"},
			{"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443"},
			{"port":8443,"protocol":"trojan","tag":"inbound-8443"},
			{"listen":"127.0.0.1","port":10085,"protocol":"vmess","tag":"internal"}
		]
	}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(panelCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, ports, err := BuildRelayConfig(path, "1.2.3.4")
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}

	wantPorts := map[int]bool{443: true, 8443: true}
	if len(ports) != len(wantPorts) {
		t.Fatalf("relayed ports = %v, want exactly %v", ports, wantPorts)
	}
	for _, p := range ports {
		if !wantPorts[p] {
			t.Errorf("unexpected relayed port %d (api/loopback should be skipped)", p)
		}
	}

	if len(cfg.InboundConfigs) != 2 {
		t.Fatalf("inbound count = %d, want 2", len(cfg.InboundConfigs))
	}
	for _, in := range cfg.InboundConfigs {
		if in.Protocol != "dokodemo-door" {
			t.Errorf("port %d: protocol = %q, want dokodemo-door", in.Port, in.Protocol)
		}
		var s struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
			Network string `json:"network"`
		}
		if err := json.Unmarshal([]byte(in.Settings), &s); err != nil {
			t.Fatalf("port %d: settings unmarshal: %v", in.Port, err)
		}
		if s.Address != "1.2.3.4" {
			t.Errorf("port %d: forward address = %q, want 1.2.3.4", in.Port, s.Address)
		}
		if s.Port != in.Port {
			t.Errorf("port %d: forward port = %d, want same port %d", in.Port, s.Port, in.Port)
		}
		if s.Network != "tcp,udp" {
			t.Errorf("port %d: network = %q, want tcp,udp", in.Port, s.Network)
		}
	}
}

func TestBuildRelayConfigNoInbounds(t *testing.T) {
	// Only an internal api inbound -> nothing to relay -> error.
	panelCfg := `{"inbounds":[{"listen":"127.0.0.1","port":62789,"protocol":"dokodemo-door","tag":"api"}]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(panelCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildRelayConfig(path, "1.2.3.4"); err == nil {
		t.Fatal("expected error when there are no relayable inbounds, got nil")
	}
}
