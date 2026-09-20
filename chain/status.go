package chain

// Status is the body of GET /chain/v1/status (§3.6) — what a hop says about
// itself to its owner or to its direct outer neighbour. It deliberately
// carries neither secrets nor the hop list: the endpoint is a health view, not
// a second way to read the document.
type Status struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Revision int64  `json:"revision"`

	LastPoll int64 `json:"lastPoll"`
	LastOk   int64 `json:"lastOk"`

	// Stale is set once chainStaleMinutes have passed without a successful
	// poll. A stale hop keeps relaying the last document it has — there is no
	// upper limit on staleness (§3.6).
	Stale bool `json:"stale"`

	Relay   StatusRelay   `json:"relay"`
	NextHop StatusNextHop `json:"nextHop"`

	// ObservedHostMismatch reports that the host the registry holds for this
	// hop is not the address its neighbour sees it come from (§4.4). It is a
	// hint for the owner, never a refusal.
	ObservedHostMismatch bool `json:"observedHostMismatch"`
}

// StatusRelay is the local relay's state: whether the xray relay process runs,
// which ports it listens on, and when it was last restarted.
type StatusRelay struct {
	Running     bool  `json:"running"`
	Ports       []int `json:"ports"`
	RestartedAt int64 `json:"restartedAt"`
}

// StatusNextHop is what the hop can say about the neighbour it polls.
type StatusNextHop struct {
	Host      string `json:"host"`
	SubPort   int    `json:"subPort"`
	Reachable bool   `json:"reachable"`
}
