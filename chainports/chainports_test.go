package chainports

import (
	"encoding/json"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// TestSkipRules pins the five rules of §3.8 one by one: they decide which of
// the panel's inbounds is reachable from outside at all, and a front that
// relays one of these opens a port nothing can ever answer on.
func TestSkipRules(t *testing.T) {
	tests := []struct {
		name string
		in   string
		skip bool
	}{
		{"plain vless inbound", `{"port":443,"protocol":"vless","tag":"inbound-443"}`, false},
		{"port zero", `{"port":0,"protocol":"vless","tag":"inbound-0"}`, true},
		{"negative port", `{"port":-1,"protocol":"vless","tag":"weird"}`, true},
		{"api inbound", `{"port":62789,"protocol":"dokodemo-door","tag":"api"}`, true},
		{"loopback ipv4", `{"listen":"127.0.0.1","port":10443,"protocol":"vless","tag":"relocated"}`, true},
		{"loopback ipv6", `{"listen":"::1","port":10443,"protocol":"vless","tag":"relocated6"}`, true},
		{"loopback by name", `{"listen":"localhost","port":10443,"protocol":"vless","tag":"named"}`, true},
		{"unix socket fallback", `{"listen":"@fallback","port":1,"protocol":"vless","tag":"fallback"}`, true},
		{"tproxy sockopt", `{"port":12345,"protocol":"dokodemo-door","tag":"awg-in","streamSettings":{"sockopt":{"tproxy":"tproxy"}}}`, true},
		{"redirect sockopt", `{"port":12346,"protocol":"dokodemo-door","tag":"wg-in","streamSettings":{"sockopt":{"tproxy":"REDIRECT"}}}`, true},
		{"followRedirect", `{"port":12347,"protocol":"dokodemo-door","tag":"dnat","settings":{"followRedirect":true}}`, true},
		{"tproxy tag suffix", `{"port":12348,"protocol":"vless","tag":"awg-tproxy-in"}`, true},
		{"port map is relayed", `{"port":8080,"protocol":"dokodemo-door","tag":"portmap"}`, false},
		{"listen on any address", `{"listen":"0.0.0.0","port":8443,"protocol":"trojan","tag":"inbound-trojan"}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var in inbound
			if err := json.Unmarshal([]byte(test.in), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			reason := skipReason(in)
			if test.skip && reason == "" {
				t.Errorf("inbound %s was relayed, want it skipped", test.in)
			}
			if !test.skip && reason != "" {
				t.Errorf("inbound %s was skipped (%s), want it relayed", test.in, reason)
			}
		})
	}
}

const sampleConfig = `{
  "log": {"loglevel": "warning"},
  "inbounds": [
    {"port": 443, "protocol": "vless", "tag": "inbound-443",
     "settings": {"clients": [{"id": "secret-uuid"}]},
     "streamSettings": {"realitySettings": {"privateKey": "do-not-copy-me"}}},
    {"listen": "0.0.0.0", "port": 8443, "protocol": "trojan", "tag": "inbound-trojan"},
    {"listen": "127.0.0.1", "port": 62789, "protocol": "dokodemo-door", "tag": "api"},
    {"listen": "127.0.0.1", "port": 10443, "protocol": "vless", "tag": "behind-nginx"},
    {"port": 12345, "protocol": "dokodemo-door", "tag": "awg-tproxy-in",
     "streamSettings": {"sockopt": {"tproxy": "tproxy"}}},
    {"port": 443, "protocol": "vless", "tag": "duplicate-443"}
  ],
  "outbounds": [{"protocol": "freedom"}]
}`

// TestBuildOnASampleConfig: the whole pipeline on one config — the ports that
// survive, in config order, with the document's own vocabulary, and nothing
// from the config's secrets anywhere near the result.
func TestBuildOnASampleConfig(t *testing.T) {
	ports, err := Build([]byte(sampleConfig))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8443, Network: chain.NetworkTCPUDP, Tag: "inbound-trojan", Source: chain.SourceXray},
	}
	if len(ports) != len(want) {
		t.Fatalf("Build returned %+v, want %+v", ports, want)
	}
	for index, port := range ports {
		if port != want[index] {
			t.Errorf("port %d = %+v, want %+v", index, port, want[index])
		}
	}
}

// TestBuildRefusesGarbage: a config that does not parse is an error, not an
// empty port list — an empty list would quietly take the whole chain off the
// air at the next revision.
func TestBuildRefusesGarbage(t *testing.T) {
	if _, err := Build([]byte("not json at all")); err == nil {
		t.Fatal("Build accepted a config that is not JSON")
	}
}

// TestBuildOnAConfigWithoutInbounds returns an empty list rather than nil, so
// a caller can append to it without thinking about it.
func TestBuildOnAConfigWithoutInbounds(t *testing.T) {
	ports, err := Build([]byte(`{"log":{"loglevel":"warning"}}`))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(ports) != 0 {
		t.Fatalf("Build returned %+v, want no ports", ports)
	}
}
