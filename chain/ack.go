package chain

import "regexp"

// OuterAck is one confirmation a hop passes inward on behalf of a neighbour
// further out (§3.3). A hop sends its own revision in X-Chain-Seen and the
// acknowledgements it has collected from its direct outer neighbours in
// X-Chain-Outer, base64 of a JSON array of these.
//
// That is how the registry learns how fresh every hop is without ever calling
// a box: the panel hears only the first inner, and everything beyond it
// arrives on that hop's own poll.
type OuterAck struct {
	Name         string `json:"name"`
	LastRevision int64  `json:"lastRevision"`
	LastSeen     int64  `json:"lastSeen"`
}

// SeenHeader, OuterHeader and ObservedHeader are the wave's request headers,
// named once so the box and the panel cannot spell them differently.
const (
	SeenHeader      = "X-Chain-Seen"
	OuterHeader     = "X-Chain-Outer"
	ObservedHeader  = "X-Chain-Observed"
	ForwardedHeader = "X-Chain-Forwarded"
)

// ObservedAddrMaxLen is how much of an observed address the registry keeps —
// the width of the column, and more than an IPv6 address with a port needs.
const ObservedAddrMaxLen = 64

// observedAddrRe is what an observed address may look like: an IP, a host, or
// either with a port, in brackets for IPv6. It travels from a neighbour in a
// header, is stored, and is later shown to the owner beside the host they
// typed (§4.4) — so it is checked at the door rather than escaped everywhere
// it is displayed.
var observedAddrRe = regexp.MustCompile(`^[0-9A-Za-z.:\[\]-]+$`)

// ObservedAddrValid reports whether value is worth storing as the address a
// join was seen to come from. An invalid one is dropped rather than repaired:
// it is a hint for the owner, and a wrong hint is worse than none.
func ObservedAddrValid(value string) bool {
	if value == "" || len(value) > ObservedAddrMaxLen {
		return false
	}
	return observedAddrRe.MatchString(value)
}
