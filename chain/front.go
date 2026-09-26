package chain

import (
	"strconv"
	"strings"
)

// Front modes a box reports (ADR 0005, #140). A box either leaves its ports as
// the relay opens them or puts nginx on 443 in front of everything; the
// panel's third mode, shared, has no meaning on a box.
const (
	FrontOff     = "off"
	FrontOnly443 = "only443"
)

// FrontHeader carries the polling hop's own front report on every poll.
const FrontHeader = "X-Chain-Front"

// FrontReport is what a box says about its front: the mode it is actually in
// and where its outer neighbours reach its sub server as a result — 443 over
// https behind the front, its own sub port otherwise.
//
// The box reports it itself because only the box knows whether its front came
// up; the panel copies SubPort and SubScheme into the registry and bumps the
// revision, so the move reaches whoever polls this box by the wave.
type FrontReport struct {
	Mode      string `json:"mode"`
	SubPort   int    `json:"subPort"`
	SubScheme string `json:"subScheme"`
}

// Header renders the report as the X-Chain-Front value:
// "mode=only443; subPort=443; subScheme=https".
func (r FrontReport) Header() string {
	return "mode=" + r.Mode + "; subPort=" + strconv.Itoa(r.SubPort) + "; subScheme=" + r.SubScheme
}

// Valid reports whether the report says something the registry can hold.
func (r FrontReport) Valid() bool {
	switch r.Mode {
	case FrontOff, FrontOnly443:
	default:
		return false
	}
	if r.SubPort < 1 || r.SubPort > 65535 {
		return false
	}
	return r.SubScheme == "http" || r.SubScheme == "https"
}

// ParseFrontReport reads an X-Chain-Front value. The header comes from another
// box and ends up in the registry, so anything short of a known mode with a
// usable port and scheme is no report at all.
func ParseFrontReport(header string) (FrontReport, bool) {
	var report FrontReport
	for _, part := range strings.Split(header, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "mode":
			report.Mode = value
		case "subPort":
			report.SubPort, _ = strconv.Atoi(value)
		case "subScheme":
			report.SubScheme = value
		}
	}
	if !report.Valid() {
		return FrontReport{}, false
	}
	return report, true
}
