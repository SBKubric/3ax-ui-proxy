package chain

import "testing"

// TestFrontReportRoundTrip: what a box says about its front in X-Chain-Front
// is what the panel (or the next hop, passing it inward) reads back.
func TestFrontReportRoundTrip(t *testing.T) {
	for _, report := range []FrontReport{
		{Mode: FrontOnly443, SubPort: 443, SubScheme: "https"},
		{Mode: FrontOff, SubPort: 2096, SubScheme: "http"},
	} {
		got, ok := ParseFrontReport(report.Header())
		if !ok || got != report {
			t.Errorf("ParseFrontReport(%q) = %+v, %t; want %+v", report.Header(), got, ok, report)
		}
	}
}

// TestParseFrontReportRefusesNonsense: the header comes from another box, and
// the panel turns it into a registry write. Anything that is not exactly a
// known mode with a usable port and scheme is no report at all.
func TestParseFrontReportRefusesNonsense(t *testing.T) {
	for _, header := range []string{
		"",
		"only443",
		"mode=shared; subPort=443; subScheme=https",
		"mode=only443; subPort=0; subScheme=https",
		"mode=only443; subPort=70000; subScheme=https",
		"mode=only443; subPort=443; subScheme=ftp",
		"mode=only443; subPort=abc; subScheme=https",
	} {
		if got, ok := ParseFrontReport(header); ok {
			t.Errorf("ParseFrontReport(%q) = %+v, accepted", header, got)
		}
	}
	// Order and spacing are not part of the format.
	if got, ok := ParseFrontReport("subScheme=https;mode=only443 ;  subPort=443"); !ok || got.Mode != FrontOnly443 || got.SubPort != 443 {
		t.Errorf("a reordered header was refused: %+v %t", got, ok)
	}
}
