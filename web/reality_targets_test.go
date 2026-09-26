package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// realityTargetRe matches one entry of REALITY_TARGETS in reality_targets.js:
// { target: 'host:port', sni: 'host' }.
var realityTargetRe = regexp.MustCompile(`\{\s*target:\s*'([^']+)',\s*sni:\s*'([^']+)'\s*\}`)

// TestRealityTargets_NoMicrosoftBrokenOnXray26 guards the random Reality
// target list the inbound modal draws from (#129): with xray 26.3.27 a Reality
// inbound targeting www.microsoft.com never completes a handshake ("REALITY:
// processed invalid connection … handshake did not complete successfully"),
// so a fresh inbound got a dead target one time in twelve. Every entry must
// also be a plain host:443 whose SNI is that host, which is what the modal
// fills into target and serverNames.
func TestRealityTargets_NoMicrosoftBrokenOnXray26(t *testing.T) {
	raw, err := fs.ReadFile(EmbeddedAssets(), "assets/js/model/reality_targets.js")
	if err != nil {
		t.Fatalf("read reality_targets.js: %v", err)
	}
	entries := realityTargetRe.FindAllStringSubmatch(string(raw), -1)
	if len(entries) == 0 {
		t.Fatal("no entries found in REALITY_TARGETS")
	}
	for _, e := range entries {
		target, sni := e[1], e[2]
		host, port, ok := strings.Cut(target, ":")
		if !ok || port != "443" || host != sni {
			t.Errorf("entry {target: %q, sni: %q}: want target <sni>:443", target, sni)
		}
		if host == "www.microsoft.com" {
			t.Errorf("entry %q: no Reality handshake with xray 26.3.27 (#129)", target)
		}
	}
}
