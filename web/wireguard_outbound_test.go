package web

import (
	"io/fs"
	"regexp"
	"testing"
)

// noKernelTunRe matches every noKernelTun value a page or model hands to xray:
// an object key (noKernelTun: false) or a constructor default (noKernelTun = false).
var noKernelTunRe = regexp.MustCompile(`noKernelTun\s*[:=]\s*(true|false)\b`)

// TestWireguardOutbounds_NewOnesUseUserspaceTun guards #131: the panel runs xray
// as root, so a WireGuard outbound with noKernelTun false gets a kernel TUN, and
// with xray 26.3.27 UDP through it fails ("use of WriteTo with pre-connected
// connection") while TCP works. Every outbound the panel creates — the WARP and
// NordVPN modals, and a new WireGuard outbound in the form — starts with
// noKernelTun true (gVisor TUN).
func TestWireguardOutbounds_NewOnesUseUserspaceTun(t *testing.T) {
	sources := []struct {
		fsys fs.FS
		path string
	}{
		{EmbeddedHTML(), "html/modals/warp_modal.html"},
		{EmbeddedHTML(), "html/modals/nord_modal.html"},
		{EmbeddedAssets(), "assets/js/model/outbound.js"},
	}
	for _, src := range sources {
		raw, err := fs.ReadFile(src.fsys, src.path)
		if err != nil {
			t.Fatalf("read %s: %v", src.path, err)
		}
		values := noKernelTunRe.FindAllStringSubmatch(string(raw), -1)
		if len(values) == 0 {
			t.Errorf("%s sets no noKernelTun", src.path)
		}
		for _, v := range values {
			if v[1] != "true" {
				t.Errorf("%s: %q, want noKernelTun true for a new WireGuard outbound", src.path, v[0])
			}
		}
	}
}

// An outbound saved before #131 without the key keeps xray's own default
// (kernel TUN) when the form reads and saves it again: the panel does not
// rewrite existing outbounds, it only hints at the switch.
func TestWireguardOutbounds_ExistingOnesKeepTheirTunMode(t *testing.T) {
	raw, err := fs.ReadFile(EmbeddedAssets(), "assets/js/model/outbound.js")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`json\.noKernelTun\s*===\s*true,`).Match(raw) {
		t.Error("Outbound.WireguardSettings.fromJson must read a missing noKernelTun as false (json.noKernelTun === true)")
	}
}
