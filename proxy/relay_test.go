package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/chainports"
	"github.com/coinman-dev/3ax-ui/v2/util/json_util"
)

func TestBuildRelayConfig(t *testing.T) {
	ports := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8443, Network: chain.NetworkTCPUDP, Tag: "inbound-8443", Source: chain.SourceXray},
	}

	cfg, relayed, err := BuildRelayConfig(ports, "1.2.3.4", "::")
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}

	wantPorts := map[int]bool{443: true, 8443: true}
	if len(relayed) != len(wantPorts) {
		t.Fatalf("relayed ports = %v, want exactly %v", relayed, wantPorts)
	}
	for _, p := range relayed {
		if !wantPorts[p] {
			t.Errorf("unexpected relayed port %d", p)
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

func TestBuildRelayConfigNoPorts(t *testing.T) {
	// Nothing to relay is an error: an xray with no inbound would start and
	// serve nobody, and the front would look healthy while it is deaf.
	if _, _, err := BuildRelayConfig(nil, "1.2.3.4", "::"); err == nil {
		t.Fatal("expected an error when there are no ports to relay, got nil")
	}
}

// TestBuildRelayConfigRefusesAnUnknownNetwork: the network names the listeners
// the relay opens, so a value it does not know would open none. The document
// comes from the panel, but a hop that trusted it blindly would relay nothing
// and say nothing.
func TestBuildRelayConfigRefusesAnUnknownNetwork(t *testing.T) {
	ports := []chain.Port{{Port: 443, Network: "quic", Tag: "inbound-443", Source: chain.SourceXray}}
	if _, _, err := BuildRelayConfig(ports, "1.2.3.4", "::"); err == nil {
		t.Fatal("a port with an unknown network was accepted")
	}
}

// TestBuildRelayConfigRefusesAPortOutOfRange: same reasoning — a port xray
// cannot bind is a dead listener.
func TestBuildRelayConfigRefusesAPortOutOfRange(t *testing.T) {
	ports := []chain.Port{{Port: 70000, Network: chain.NetworkTCP, Tag: "x", Source: chain.SourceExtra}}
	if _, _, err := BuildRelayConfig(ports, "1.2.3.4", "::"); err == nil {
		t.Fatal("a port out of range was accepted")
	}
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

// TestBuildRelayConfigKeepsEachPortsNetwork: a UDP-only tunnel port and a
// TCP-only one must not both become tcp,udp listeners — the network travels
// per port in the document for exactly this reason.
func TestBuildRelayConfigKeepsEachPortsNetwork(t *testing.T) {
	ports := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg},
		{Port: 8443, Network: chain.NetworkTCP, Tag: "extra-8443", Source: chain.SourceExtra},
	}
	cfg, relayed, err := BuildRelayConfig(ports, "1.2.3.4", "::")
	if err != nil {
		t.Fatalf("BuildRelayConfig: %v", err)
	}
	if len(relayed) != 3 {
		t.Fatalf("relayed ports = %v, want 443, 51820 and 8443", relayed)
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

// TestBuildRelayConfigRefusesADuplicatePort: one port, one source (§3.8) —
// merging them silently would make the front relay one of two services and
// leave the other dark with nothing said.
func TestBuildRelayConfigRefusesADuplicatePort(t *testing.T) {
	ports := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 443, Network: chain.NetworkUDP, Tag: "extra-443", Source: chain.SourceExtra},
	}
	_, _, err := BuildRelayConfig(ports, "1.2.3.4", "::")
	if err == nil {
		t.Fatal("a port claimed by two sources was accepted")
	}
	for _, want := range []string{"xray", "extra"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the source %q", err, want)
		}
	}
}

// TestPortsComputedByThePanelReachTheRelay pins the contract between the two
// halves of §3.8: what chainports computes on the panel is what the hop
// relays, with no translation in between — the chain document is the only
// carrier.
func TestPortsComputedByThePanelReachTheRelay(t *testing.T) {
	raw := `{"inbounds":[
	  {"listen":"127.0.0.1","port":62789,"protocol":"tunnel","tag":"api"},
	  {"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443","settings":{"clients":[{"id":"x"}]},
	   "streamSettings":{"security":"reality","realitySettings":{"privateKey":"SECRET"}}},
	  {"listen":"::","port":12345,"protocol":"dokodemo-door","tag":"awg-tproxy-in",
	   "settings":{"followRedirect":true},"streamSettings":{"sockopt":{"tproxy":"tproxy"}}}]}`
	computed, err := chainports.Build([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	// What the panel adds outside the xray config travels in the same list.
	computed = append(computed, chain.Port{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg})

	_, relayed, err := BuildRelayConfig(computed, "203.0.113.1", "0.0.0.0")
	if err != nil {
		t.Fatalf("BuildRelayConfig over a computed port list: %v", err)
	}
	if len(relayed) != 2 || relayed[0] != 443 || relayed[1] != 51820 {
		t.Fatalf("relayed ports = %v, want [443 51820]", relayed)
	}
}
