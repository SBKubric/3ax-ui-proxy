package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/util/json_util"
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

	cfg, ports, err := BuildRelayConfig(path, "1.2.3.4", "::", nil)
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
		if string(in.Listen) != `"::"` {
			t.Errorf("port %d: listen = %s, want dual-stack \"::\"", in.Port, in.Listen)
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
	if _, _, err := BuildRelayConfig(path, "1.2.3.4", "::", nil); err == nil {
		t.Fatal("expected error when there are no relayable inbounds, got nil")
	}
}

func writePanelCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func relaySettings(t *testing.T, in json_util.RawMessage) (port int, network string) {
	t.Helper()
	var s struct {
		Port    int    `json:"port"`
		Network string `json:"network"`
	}
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatalf("settings unmarshal: %v", err)
	}
	return s.Port, s.Network
}

func TestBuildRelayConfigExtraPorts(t *testing.T) {
	// An AmneziaWG listener lives on the host, not in the xray config, so the
	// panel config alone yields one relayed port; extraPorts adds the UDP one
	// with its own network selector and the TCP-only one likewise.
	path := writePanelCfg(t, `{"inbounds":[{"port":443,"protocol":"vless","tag":"inbound-443"}]}`)
	extra := []ExtraPort{{Port: 51820, Network: "udp"}, {Port: 8443, Network: "tcp"}}

	cfg, ports, err := BuildRelayConfig(path, "1.2.3.4", "::", extra)
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}
	if len(ports) != 3 {
		t.Fatalf("relayed ports = %v, want 443, 51820 and 8443", ports)
	}
	want := map[int]string{443: "tcp,udp", 51820: "udp", 8443: "tcp"}
	for _, in := range cfg.InboundConfigs {
		fwdPort, network := relaySettings(t, in.Settings)
		if fwdPort != in.Port {
			t.Errorf("port %d: forward port = %d, want same port", in.Port, fwdPort)
		}
		if network != want[in.Port] {
			t.Errorf("port %d: network = %q, want %q", in.Port, network, want[in.Port])
		}
		delete(want, in.Port)
	}
	if len(want) != 0 {
		t.Errorf("ports missing from relay config: %v", want)
	}
}

func TestBuildRelayConfigExtraPortsOnly(t *testing.T) {
	// A panel whose only inbound is internal still relays when extraPorts is set.
	path := writePanelCfg(t, `{"inbounds":[{"listen":"127.0.0.1","port":62789,"protocol":"dokodemo-door","tag":"api"}]}`)
	_, ports, err := BuildRelayConfig(path, "1.2.3.4", "::", []ExtraPort{{Port: 51820, Network: "udp"}})
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}
	if len(ports) != 1 || ports[0] != 51820 {
		t.Fatalf("relayed ports = %v, want [51820]", ports)
	}
}

func TestBuildRelayConfigSkipsTransparentInbounds(t *testing.T) {
	// The panel adds a synthetic dokodemo-door TPROXY inbound per tunnel routed
	// via Xray. It listens on "::" like a public inbound but only ever receives
	// kernel-redirected packets, so it must not become a relayed port. Both the
	// semantic markers and the default tag suffix identify it.
	panelCfg := `{
		"inbounds": [
			{"port":443,"protocol":"vless","tag":"inbound-443"},
			{"listen":"::","port":12345,"protocol":"dokodemo-door","tag":"my-tunnel",
			 "settings":{"network":"tcp,udp","followRedirect":true},
			 "streamSettings":{"sockopt":{"tproxy":"tproxy"}}},
			{"listen":"::","port":12346,"protocol":"dokodemo-door","tag":"redir",
			 "settings":{"followRedirect":true}},
			{"listen":"::","port":12347,"protocol":"dokodemo-door","tag":"renamed",
			 "streamSettings":{"sockopt":{"tproxy":"redirect"}}},
			{"listen":"::","port":12348,"protocol":"dokodemo-door","tag":"wg-tproxy-in"}
		]
	}`
	path := writePanelCfg(t, panelCfg)
	_, ports, err := BuildRelayConfig(path, "1.2.3.4", "::", nil)
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}
	if len(ports) != 1 || ports[0] != 443 {
		t.Fatalf("relayed ports = %v, want [443] only (TPROXY inbounds skipped)", ports)
	}
}

func TestSkipReason(t *testing.T) {
	plain := panelInbound{Port: 443, Protocol: "vless", Tag: "inbound-443"}
	if r := skipReason(plain); r != "" {
		t.Errorf("public inbound skipped: %q", r)
	}
	// A plain dokodemo-door port map (no redirect, no tproxy) is a real public
	// inbound and must still be relayed.
	portMap := panelInbound{Port: 8080, Protocol: "dokodemo-door", Tag: "portmap"}
	if r := skipReason(portMap); r != "" {
		t.Errorf("dokodemo port map skipped: %q", r)
	}
	tagged := panelInbound{Port: 12345, Protocol: "dokodemo-door", Tag: "awg-tproxy-in"}
	if r := skipReason(tagged); r == "" {
		t.Error("awg-tproxy-in not skipped")
	}
}

func TestBuildRelayConfigExtraPortCollision(t *testing.T) {
	// One port, one source: an extra port that is also an xray inbound is a
	// config mistake, not something to merge silently.
	path := writePanelCfg(t, `{"inbounds":[{"port":443,"protocol":"vless","tag":"inbound-443"}]}`)
	if _, _, err := BuildRelayConfig(path, "1.2.3.4", "::", []ExtraPort{{Port: 443, Network: "udp"}}); err == nil {
		t.Fatal("expected error when an extra port collides with an xray inbound port, got nil")
	}
}
