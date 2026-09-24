package tunnel

import (
	"strings"
	"testing"
)

// The stand's server (SBKubric/3ax-ui-proxy#118): S4 = 22 and the legacy 1420
// put a full-size packet at 1502 bytes on a 1500-byte link, so TLS stalled
// after TCP had connected.
func TestDefaultMTULeavesRoomForS4Padding(t *testing.T) {
	cases := []struct {
		s4, def, max int
	}{
		{s4: 0, def: 1420, max: 1440}, // no padding: WireGuard's own numbers
		{s4: 22, def: 1398, max: 1418},
		{s4: 32, def: 1388, max: 1408}, // the largest S4 the kernel accepts
		{s4: -1, def: 1420, max: 1440}, // garbage never raises the MTU
	}
	for _, c := range cases {
		if got := DefaultMTU(c.s4); got != c.def {
			t.Errorf("DefaultMTU(%d) = %d, want %d", c.s4, got, c.def)
		}
		if got := MaxMTU(c.s4); got != c.max {
			t.Errorf("MaxMTU(%d) = %d, want %d", c.s4, got, c.max)
		}
	}

	// Spelled out once more from the wire format, so the constants cannot drift
	// together: outer IP + UDP + S4 + message header + inner packet + tag.
	for s4 := 0; s4 <= 32; s4++ {
		if n := 40 + 8 + s4 + 16 + DefaultMTU(s4) + 16; n != 1500 {
			t.Errorf("S4=%d: a full-size packet is %d bytes over IPv6, want 1500", s4, n)
		}
		if n := 20 + 8 + s4 + 16 + MaxMTU(s4) + 16; n != 1500 {
			t.Errorf("S4=%d: a full-size packet at MaxMTU is %d bytes over IPv4, want 1500", s4, n)
		}
	}
}

func TestFollowDefaultMTU(t *testing.T) {
	cases := []struct {
		name            string
		mtu, prevS4, s4 int
		want            int
	}{
		{"legacy default follows the padding", 1420, 0, 22, 1398},
		{"legacy default stays without padding", 1420, 0, 0, 1420},
		{"default for the old padding follows the new one", 1398, 22, 10, 1410},
		{"default goes back to 1420 when padding is removed", 1398, 22, 0, 1420},
		{"unchanged padding keeps the default", 1398, 22, 22, 1398},
		{"operator value is kept", 1280, 22, 10, 1280},
		{"operator value is kept even if too large", 1450, 0, 22, 1450},
		{"unset gets a default once padding is on", 0, 0, 22, 1398},
		{"unset stays unset without padding", 0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := FollowDefaultMTU(c.mtu, c.prevS4, c.s4); got != c.want {
			t.Errorf("%s: FollowDefaultMTU(%d, %d, %d) = %d, want %d", c.name, c.mtu, c.prevS4, c.s4, got, c.want)
		}
	}
}

func TestFollowServerMTUIgnoresS4OnWireGuard(t *testing.T) {
	s := &Server{MTU: 1420, S4: 22} // stray S4 on a WireGuard record
	FollowServerMTU(WG, s, 0)
	if s.MTU != 1420 {
		t.Errorf("WireGuard MTU = %d, want 1420: native WireGuard has no S4 padding", s.MTU)
	}
	a := &Server{MTU: 1420, S4: 22}
	FollowServerMTU(AWG, a, 0)
	if a.MTU != 1398 {
		t.Errorf("AmneziaWG MTU = %d, want 1398", a.MTU)
	}
}

func TestValidateMTU_RejectsWhatThePaddingCannotFit(t *testing.T) {
	cases := []struct {
		name string
		k    Kind
		mtu  int
		s4   int
		ok   bool
	}{
		{"stand bug: 1420 with S4=22", AWG, 1420, 22, false},
		{"IPv4 ceiling with S4=22", AWG, 1418, 22, true},
		{"default with S4=22", AWG, 1398, 22, true},
		{"unset", AWG, 0, 22, true},
		{"1420 without padding", AWG, 1420, 0, true},
		{"WireGuard ceiling", WG, 1440, 0, true},
		{"WireGuard over the ceiling", WG, 1441, 0, false},
		{"WireGuard ignores S4", WG, 1440, 22, true},
	}
	for _, c := range cases {
		err := ValidateMTU(c.k, &Server{MTU: c.mtu, S4: c.s4})
		if (err == nil) != c.ok {
			t.Errorf("%s: ValidateMTU(MTU=%d, S4=%d) = %v, want ok=%v", c.name, c.mtu, c.s4, err, c.ok)
		}
	}

	err := ValidateMTU(AWG, &Server{MTU: 1420, S4: 22})
	for _, want := range []string{"1502 bytes", "at most 1418", "1398"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not say %q", err, want)
		}
	}
}
