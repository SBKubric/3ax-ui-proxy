// Package chain holds the wire types of the proxy chain: the chain document a
// hop polls from its next hop, the status it reports, and the secret/name
// rules both ends must agree on (docs/spec/proxy-chain.md §3, §4).
//
// It is deliberately free of panel dependencies — no web/, no database/ — so
// the box side (package proxy) and the panel side (web/service, sub) can share
// exactly one definition of the wire format instead of two that drift apart.
package chain

// DocumentVersion is the only document version this code speaks. It travels in
// the document so a box that meets a newer panel can refuse loudly instead of
// applying half a format it does not understand.
const DocumentVersion = 1

// Roles a hop can have: an inner front relays deeper into the chain, an edge
// front is what clients connect to.
const (
	RoleInner = "inner"
	RoleEdge  = "edge"
)

// Hop states. pending means the owner created the registry entry and handed
// out a join token, but no box has entered yet; joined means it has; legacy is
// the single hop imported from the old proxyOverrideHost setting (§2.3);
// draining means the owner deleted it and it is serving its former outer
// neighbours until they have re-chained past it (§4.5).
const (
	StatePending  = "pending"
	StateJoined   = "joined"
	StateLegacy   = "legacy"
	StateDraining = "draining"
)

// Networks a relayed port carries. "tcp,udp" is one value, not two: the relay
// config needs both listeners on the same port.
const (
	NetworkTCP    = "tcp"
	NetworkUDP    = "udp"
	NetworkTCPUDP = "tcp,udp"
)

// Where the panel learned a relayed port from (§3.8). extra is the operator's
// own chainExtraPorts list, everything else the panel computes itself.
const (
	SourceXray    = "xray"
	SourceAwg     = "awg"
	SourceWg      = "wg"
	SourceMtproto = "mtproto"
	SourceExtra   = "extra"
)

// Document is the truncated view of the chain one hop receives from its next
// hop (§3.1). It carries everything outward from that hop and nothing inward
// except the single address in NextHop.
type Document struct {
	Version     int     `json:"version"`
	Revision    int64   `json:"revision"`
	GeneratedAt int64   `json:"generatedAt"`
	Self        Self    `json:"self"`
	NextHop     NextHop `json:"nextHop"`

	// ActiveEdge is present for every inner, and for an edge only when that
	// edge is itself the active one (§3.2): a seized standby edge must not
	// learn the name of its active neighbour.
	ActiveEdge string `json:"activeEdge,omitempty"`

	Hops  []Hop  `json:"hops"`
	Ports []Port `json:"ports"`
}

// Self is how the registry names the hop reading this document.
//
// State is the one field the draining design added to the wire (§4.5.3): a hop
// that reads draining here hands its own NextHop to its outer neighbours
// instead of itself, so they re-chain past it while it is still serving them.
// An absent State reads as joined — a box older than the panel simply does not
// know how to drain.
//
// RealityTarget and RealityServerName are this edge's neighbour target (ADR
// 0005): the site next to its address that an unknown SNI is passed on to,
// and the one name its front hands to the Reality inbounds. The server name
// is always spelled out, the registry's default already applied. Both are
// absent for an inner and for an edge that has none, so a box older than the
// fields reads the document exactly as before.
type Self struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Host  string `json:"host"`
	State string `json:"state,omitempty"`

	RealityTarget     string `json:"realityTarget,omitempty"`
	RealityServerName string `json:"realityServerName,omitempty"`
}

// Draining reports whether this hop is on its way out of the chain (§4.5.3).
func (s Self) Draining() bool { return s.State == StateDraining }

// NextHop is the one address a hop knows towards the real server, plus the
// subscription paths it must proxy. The paths travel in the document so
// proxy.json does not have to store them.
type NextHop struct {
	Host      string `json:"host"`
	SubPort   int    `json:"subPort"`
	SubScheme string `json:"subScheme"`
	SubPath   string `json:"subPath"`
	JsonPath  string `json:"jsonPath"`
	TunPath   string `json:"tunPath"`
}

// Hop is one entry of the outward-truncated hop list. SecretHash is the sha256
// hex of that hop's hop secret: a hop authorises its direct outer neighbours by
// comparing against these, and never sees a secret in the clear.
//
// An edge entry carries that edge's neighbour target as Self does: an inner's
// front passes every edge's server name through to the Reality inbounds, so
// it needs all of them. Optional, like Self's.
type Hop struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	Host       string `json:"host"`
	SubPort    int    `json:"subPort"`
	SecretHash string `json:"secretHash"`
	State      string `json:"state"`

	RealityTarget     string `json:"realityTarget,omitempty"`
	RealityServerName string `json:"realityServerName,omitempty"`
}

// Port is one relayed port, identical in every document of the chain: the real
// server's ports are forwarded one to one all the way to the edge.
type Port struct {
	Port    int    `json:"port"`
	Network string `json:"network"`
	Tag     string `json:"tag"`
	Source  string `json:"source"`
}
