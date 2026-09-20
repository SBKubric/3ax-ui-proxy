package model

import "github.com/coinman-dev/3ax-ui/v2/chain"

// The chain registry (docs/spec/proxy-chain.md §2.2). One table, because the
// registry needs a unique name, lookups by next_hop_id and indexes — none of
// which a JSON blob in settings gives — and because ADR 0002 keeps upstream
// models untouched.
//
// Roles and states are the wire values of package chain: the registry, the
// document and the box all have to agree on the very same strings, so there is
// one definition and these are aliases of it.
const (
	ChainRoleInner = chain.RoleInner
	ChainRoleEdge  = chain.RoleEdge

	ChainStatePending = chain.StatePending
	ChainStateJoined  = chain.StateJoined
	ChainStateLegacy  = chain.StateLegacy
)

// ChainHop is one hop of the chain registry. Times are int64 milliseconds UTC,
// as everywhere else in the panel.
//
// SQLite index names are global, hence the idx_chain_ prefix and the
// registration in namedIndexes (database/db.go). No foreign key on
// next_hop_id: deletion re-chains the neighbours in the service (§4.5), and a
// cascade there would take the rest of the chain with it.
type ChainHop struct {
	Id   int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name string `json:"name" gorm:"size:32;not null;uniqueIndex:idx_chain_hops_name"`
	Host string `json:"host" gorm:"size:255;not null"`
	Role string `json:"role" gorm:"size:8;not null;index:idx_chain_hops_role,priority:1"` // inner | edge

	NextHopId *int   `json:"nextHopId" gorm:"index:idx_chain_hops_next"` // nil = the panel (real server)
	Position  int    `json:"position" gorm:"not null;default:0"`         // order among the inner fronts
	SubPort   int    `json:"subPort" gorm:"not null;default:2096"`       // port of the wave and of this hop's subscriptions
	SubScheme string `json:"subScheme" gorm:"size:8;not null;default:https"`

	State    string `json:"state" gorm:"size:8;not null;index:idx_chain_hops_role,priority:2"` // pending|joined|legacy
	IsActive bool   `json:"isActive" gorm:"not null;default:false"`

	// Secrets are stored hashed only: the clear join token is shown to the
	// owner once, the clear hop secret lives on the box (§2.2).
	JoinTokenHash    string `json:"-" gorm:"size:64"` // sha256 hex, emptied once used
	JoinTokenExpires int64  `json:"joinTokenExpires"`
	SecretHash       string `json:"-" gorm:"size:64"` // sha256 hex of the hop secret

	ObservedAddr string `json:"observedAddr" gorm:"size:64"` // address the join arrived from (a hint, §4.4)
	JoinedAt     int64  `json:"joinedAt"`
	LastSeenAt   int64  `json:"lastSeenAt"`   // last confirmed poll
	LastRevision int64  `json:"lastRevision"` // last revision the hop confirmed
	CreatedAt    int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt    int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

// TableName pins the table name so a future rename of the struct cannot
// silently migrate the registry into a second, empty table.
func (ChainHop) TableName() string {
	return "chain_hops"
}
