package chain

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
