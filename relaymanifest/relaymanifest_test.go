package relaymanifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawPanelConfig is the shape the panel writes to bin/config.json on the
// stand: the gRPC api tunnel, a VLESS-Reality inbound with its private key
// and clients, and the synthetic TPROXY inbound for AmneziaWG.
const rawPanelConfig = `{
  "log": {"loglevel": "warning"},
  "api": {"tag": "api", "services": ["HandlerService", "StatsService"]},
  "inbounds": [
    {"listen": "127.0.0.1", "port": 62789, "protocol": "tunnel", "tag": "api",
     "settings": {"address": "127.0.0.1"}},
    {"listen": "0.0.0.0", "port": 443, "protocol": "vless", "tag": "inbound-443",
     "settings": {"clients": [{"id": "a5899a9e-dc44-4ec4-bba1-72b6e29bd2d1", "email": "stand-client", "flow": "xtls-rprx-vision"}],
                  "decryption": "none"},
     "streamSettings": {"network": "tcp", "security": "reality",
                        "realitySettings": {"privateKey": "SECRET-REALITY-PRIVATE-KEY", "shortIds": ["abcd"], "dest": "www.apple.com:443"}},
     "sniffing": {"enabled": true, "destOverride": ["http", "tls"]}},
    {"listen": "::", "port": 12345, "protocol": "dokodemo-door", "tag": "awg-tproxy-in",
     "settings": {"network": "tcp,udp", "followRedirect": true},
     "streamSettings": {"sockopt": {"tproxy": "tproxy"}}}
  ],
  "outbounds": [{"protocol": "wireguard", "tag": "warp", "settings": {"secretKey": "SECRET-WARP-KEY"}}],
  "routing": {"rules": []}
}`

func TestBuildKeepsOnlyRelayFields(t *testing.T) {
	m, err := Build([]byte(rawPanelConfig), "1.8.1.3", time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := m.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	for _, secret := range []string{"SECRET-REALITY-PRIVATE-KEY", "SECRET-WARP-KEY", "a5899a9e", "stand-client", "privateKey", "clients", "decryption", "outbounds", "routing", "sniffing"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("manifest leaks %q:\n%s", secret, out)
		}
	}

	// Independent expectation of the whole document, not a re-run of Build.
	var got, want any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	const wantJSON = `{
	  "relayManifest": {"version": 1, "generatedAt": "2026-09-12T10:00:00Z", "panelVersion": "1.8.1.3"},
	  "inbounds": [
	    {"listen": "127.0.0.1", "port": 62789, "protocol": "tunnel", "tag": "api"},
	    {"listen": "0.0.0.0", "port": 443, "protocol": "vless", "tag": "inbound-443"},
	    {"listen": "::", "port": 12345, "protocol": "dokodemo-door", "tag": "awg-tproxy-in",
	     "settings": {"followRedirect": true},
	     "streamSettings": {"sockopt": {"tproxy": "tproxy"}}}
	  ]
	}`
	json.Unmarshal([]byte(wantJSON), &want)
	gotB, _ := json.Marshal(got)
	wantB, _ := json.Marshal(want)
	if string(gotB) != string(wantB) {
		t.Fatalf("manifest differs from expected\n got: %s\nwant: %s", gotB, wantB)
	}
}

func TestValidateRejectsRawPanelConfig(t *testing.T) {
	_, err := Validate([]byte(rawPanelConfig))
	if err == nil {
		t.Fatal("a raw bin/config.json was accepted as a relay manifest")
	}
	for _, want := range []string{"not a relay manifest", "x-ui relay-manifest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestValidateAcceptsBuiltManifest(t *testing.T) {
	built, _ := Build([]byte(rawPanelConfig), "1.8.1.3", time.Now())
	data, _ := built.JSON()
	m, err := Validate(data)
	if err != nil {
		t.Fatalf("Validate rejected Build's own output: %v", err)
	}
	if len(m.Inbounds) != 3 || m.Inbounds[1].Port != 443 {
		t.Fatalf("validated manifest lost content: %+v", m.Inbounds)
	}
}

func TestValidateRejectsSecretsSmuggledIntoAManifest(t *testing.T) {
	// Marker present, but an inbound carries a field outside the whitelist:
	// the document may have been hand-edited from a raw config.
	doc := `{"relayManifest":{"version":1},"inbounds":[{"port":443,"protocol":"vless","streamSettings":{"realitySettings":{"privateKey":"x"}}}]}`
	if _, err := Validate([]byte(doc)); err == nil || !strings.Contains(err.Error(), "realitySettings") {
		t.Fatalf("want an error naming the offending field, got %v", err)
	}
}

func TestValidateRejectsMissingOrForeignMarker(t *testing.T) {
	cases := map[string]string{
		"no marker":      `{"inbounds":[{"port":443,"protocol":"vless"}]}`,
		"future version": `{"relayManifest":{"version":2},"inbounds":[{"port":443,"protocol":"vless"}]}`,
		"no inbounds":    `{"relayManifest":{"version":1}}`,
		"not json":       `[Interface]`,
	}
	for name, doc := range cases {
		if _, err := Validate([]byte(doc)); err == nil {
			t.Errorf("%s: accepted %s", name, doc)
		}
	}
}

func TestFromFileBuildsFromTheXrayConfigOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(rawPanelConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := FromFile(path, "1.8.1.3")
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if _, err := Validate(out); err != nil {
		t.Fatalf("FromFile output is not a valid manifest: %v", err)
	}
	if strings.Contains(string(out), "SECRET-REALITY-PRIVATE-KEY") {
		t.Fatal("FromFile leaked the Reality private key")
	}

	_, err = FromFile(filepath.Join(dir, "missing.json"), "1.8.1.3")
	if err == nil || !strings.Contains(err.Error(), "missing.json") {
		t.Fatalf("missing config must name the path, got %v", err)
	}
}
