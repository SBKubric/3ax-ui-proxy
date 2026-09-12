package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExtraPort(t *testing.T) {
	cases := []struct {
		in      string
		want    ExtraPort
		wantErr bool
	}{
		{in: "51820/udp", want: ExtraPort{Port: 51820, Network: "udp"}},
		{in: " 8443/TCP ", want: ExtraPort{Port: 8443, Network: "tcp"}},
		{in: "5000/tcp+udp", want: ExtraPort{Port: 5000, Network: "tcp,udp"}},
		{in: "51820", wantErr: true},         // suffix is mandatory
		{in: "51820/sctp", wantErr: true},    // unknown protocol
		{in: "0/udp", wantErr: true},         // out of range
		{in: "70000/udp", wantErr: true},     // out of range
		{in: "abc/udp", wantErr: true},       // not a number
		{in: "5000-5010/udp", wantErr: true}, // ranges are not supported
	}
	for _, c := range cases {
		got, err := ParseExtraPort(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseExtraPort(%q) = %+v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseExtraPort(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseExtraPort(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func writeProxyCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigExtraPorts(t *testing.T) {
	path := writeProxyCfg(t, `{"upstreamHost":"1.2.3.4","relayManifestPath":"/etc/x-ui/relay-manifest.json","extraPorts":["51820/udp","","8443/tcp"]}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got := cfg.ExtraRelayPorts()
	want := []ExtraPort{{Port: 51820, Network: "udp"}, {Port: 8443, Network: "tcp"}}
	if len(got) != len(want) {
		t.Fatalf("extra ports = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extra port %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadConfigExtraPortsRejectsBadEntries(t *testing.T) {
	for _, body := range []string{
		`{"upstreamHost":"1.2.3.4","relayManifestPath":"/x","extraPorts":["51820"]}`,
		`{"upstreamHost":"1.2.3.4","relayManifestPath":"/x","extraPorts":["51820/udp","51820/tcp"]}`,
	} {
		if _, err := LoadConfig(writeProxyCfg(t, body)); err == nil {
			t.Errorf("LoadConfig(%s): expected error, got nil", body)
		}
	}
}

// TestLoadConfigRequiresRelayManifestPath: the input was renamed without an
// alias (ADR 0001) — a proxy.json still saying xrayConfigPath must fail
// loudly and name the new key, not silently run with no relay input.
func TestLoadConfigRequiresRelayManifestPath(t *testing.T) {
	_, err := LoadConfig(writeProxyCfg(t, `{"upstreamHost":"1.2.3.4","xrayConfigPath":"/etc/x-ui/panel-xray.json"}`))
	if err == nil || !strings.Contains(err.Error(), "relayManifestPath") {
		t.Fatalf("want an error naming relayManifestPath, got %v", err)
	}
}
