package chainports

import (
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// TestSkipRules pins the five rules of §3.8 one by one: they decide which of
// the panel's inbounds is reachable from outside at all, and a front that
// relays one of these opens a port nothing can ever answer on.
func TestSkipRules(t *testing.T) {
	tests := []struct {
		name string
		in   Inbound
		skip bool
	}{
		{"plain vless inbound", Inbound{Port: 443, Protocol: "vless", Tag: "inbound-443"}, false},
		{"port zero", Inbound{Port: 0, Protocol: "vless", Tag: "inbound-0"}, true},
		{"negative port", Inbound{Port: -1, Protocol: "vless", Tag: "weird"}, true},
		{"api inbound", Inbound{Port: 62789, Protocol: "dokodemo-door", Tag: "api"}, true},
		{"loopback ipv4", Inbound{Listen: "127.0.0.1", Port: 10443, Protocol: "vless", Tag: "relocated"}, true},
		{"loopback ipv6", Inbound{Listen: "::1", Port: 10443, Protocol: "vless", Tag: "relocated6"}, true},
		{"loopback by name", Inbound{Listen: "localhost", Port: 10443, Protocol: "vless", Tag: "named"}, true},
		{"unix socket fallback", Inbound{Listen: "@fallback", Port: 1, Protocol: "vless", Tag: "fallback"}, true},
		{"tproxy sockopt", Inbound{Port: 12345, Protocol: "dokodemo-door", Tag: "awg-in",
			StreamSettings: `{"sockopt":{"tproxy":"tproxy"}}`}, true},
		{"redirect sockopt", Inbound{Port: 12346, Protocol: "dokodemo-door", Tag: "wg-in",
			StreamSettings: `{"sockopt":{"tproxy":"REDIRECT"}}`}, true},
		{"followRedirect", Inbound{Port: 12347, Protocol: "dokodemo-door", Tag: "dnat",
			Settings: `{"followRedirect":true}`}, true},
		{"tproxy tag suffix", Inbound{Port: 12348, Protocol: "vless", Tag: "awg-tproxy-in"}, true},
		{"port map is relayed", Inbound{Port: 8080, Protocol: "dokodemo-door", Tag: "portmap"}, false},
		{"listen on any address", Inbound{Listen: "0.0.0.0", Port: 8443, Protocol: "trojan", Tag: "inbound-trojan"}, false},
		{"unparsable stream settings are no reason to skip", Inbound{Port: 8444, Protocol: "vless",
			Tag: "broken", StreamSettings: `{oops`}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason := SkipReason(test.in)
			if test.skip && reason == "" {
				t.Errorf("inbound %+v was relayed, want it skipped", test.in)
			}
			if !test.skip && reason != "" {
				t.Errorf("inbound %+v was skipped (%s), want it relayed", test.in, reason)
			}
		})
	}
}

// TestPortsOverASampleTable: the whole pipeline on one set of inbounds — the
// ports that survive, in table order, with the document's own vocabulary.
func TestPortsOverASampleTable(t *testing.T) {
	ports := Ports([]Inbound{
		{Port: 443, Protocol: "vless", Tag: "inbound-443",
			Settings:       `{"clients":[{"id":"secret-uuid"}]}`,
			StreamSettings: `{"realitySettings":{"privateKey":"do-not-copy-me"}}`},
		{Listen: "0.0.0.0", Port: 8443, Protocol: "trojan", Tag: "inbound-trojan"},
		{Listen: "127.0.0.1", Port: 62789, Protocol: "dokodemo-door", Tag: "api"},
		{Listen: "127.0.0.1", Port: 10443, Protocol: "vless", Tag: "behind-nginx"},
		{Port: 12345, Protocol: "dokodemo-door", Tag: "awg-tproxy-in",
			StreamSettings: `{"sockopt":{"tproxy":"tproxy"}}`},
		{Port: 443, Protocol: "vless", Tag: "duplicate-443"},
	})
	want := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8443, Network: chain.NetworkTCPUDP, Tag: "inbound-trojan", Source: chain.SourceXray},
	}
	if len(ports) != len(want) {
		t.Fatalf("Ports() = %+v, want %+v", ports, want)
	}
	for index, port := range ports {
		if port != want[index] {
			t.Errorf("port %d = %+v, want %+v", index, port, want[index])
		}
	}
}

// TestPortsOfNothing: an empty table is an empty list, not a nil surprise for
// the JSON the document carries.
func TestPortsOfNothing(t *testing.T) {
	ports := Ports(nil)
	if ports == nil {
		t.Fatal("Ports(nil) = nil, want an empty list")
	}
	if len(ports) != 0 {
		t.Fatalf("Ports(nil) = %+v, want an empty list", ports)
	}
}
