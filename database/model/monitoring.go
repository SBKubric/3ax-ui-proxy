package model

// Monitoring tables (docs/spec/monitoring-panel.md §2.1). The panel is a
// passive receiver: mon-server pushes target transitions and 5-minute
// aggregates, the panel stores them and renders them. Every time is UTC
// milliseconds, every latency an integer millisecond, NULL when no probe
// succeeded in the bucket.
//
// Index names carry the idx_mon_ prefix and are registered in
// database.namedIndexes: SQLite keeps index names in one namespace for the
// whole file, so a name must say which table it belongs on.

// Monitoring inbound kinds. AWG is addressed as kind "awg" with inboundId 0 so
// xray inbounds and the AmneziaWG server share one target key.
const (
	MonKindXray = "xray"
	MonKindAwg  = "awg"
)

// Target states, as decided by mon-server. STALE is not a target state: it is
// the panel's own flag, kept in memory, and never written to a row.
const (
	MonStateUp       = "UP"
	MonStateDown     = "DOWN"
	MonStateFlapping = "FLAPPING"
	MonStateUnknown  = "UNKNOWN"
	MonStatePaused   = "PAUSED"
)

// Registry states of a mon-client, as mon-server reports them.
const (
	MonClientOnline  = "ONLINE"
	MonClientOffline = "OFFLINE"
)

// Event kinds.
const (
	MonEventTarget    = "target"
	MonEventMonClient = "mon_client"
	MonEventPanel     = "panel"
)

// Paths a target is probed over.
const (
	MonPathDirect = "direct"
	MonPathProxy  = "proxy"
)

// MonBucketMs is the aggregate bucket mon-server sends in v1 (5 minutes).
const MonBucketMs int64 = 300_000

// MonTarget is the last known state of one (mon-client, inbound, path). A row
// appears with the first event or aggregate for its key and disappears when
// its mon-client leaves the registry snapshot or the inbound is deleted.
type MonTarget struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	MonClientId string `json:"monClientId" gorm:"size:64;uniqueIndex:idx_mon_target_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:8;uniqueIndex:idx_mon_target_key,priority:2;index:idx_mon_target_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"uniqueIndex:idx_mon_target_key,priority:3;index:idx_mon_target_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:8;uniqueIndex:idx_mon_target_key,priority:4"`
	State       string `json:"state" gorm:"size:16"`
	Since       int64  `json:"since"`
	Reason      string `json:"reason"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// TableName pins the name the spec uses.
func (MonTarget) TableName() string { return "mon_targets" }

// MonEvent is one transition in the event feed. Id is the UUID v7 mon-server
// generated, which is also the deduplication key. FromState/ToState are the
// wire fields "from"/"to": those words are SQL keywords, so the columns carry
// a suffix and only the JSON keeps the contract's spelling.
type MonEvent struct {
	Id          string `json:"id" gorm:"primaryKey;size:36"`
	Ts          int64  `json:"ts" gorm:"index:idx_mon_event_ts;index:idx_mon_event_inbound_ts,priority:3"`
	ReceivedAt  int64  `json:"receivedAt"`
	Kind        string `json:"kind" gorm:"size:16"`
	MonClientId string `json:"monClientId" gorm:"size:64"`
	InboundKind string `json:"inboundKind" gorm:"size:8;index:idx_mon_event_inbound_ts,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"index:idx_mon_event_inbound_ts,priority:2"`
	Path        string `json:"path" gorm:"size:8"`
	FromState   string `json:"from" gorm:"column:from_state;size:16"`
	ToState     string `json:"to" gorm:"column:to_state;size:16"`
	Reason      string `json:"reason"`
	Notified    bool   `json:"notified"`
}

// TableName pins the name the spec uses.
func (MonEvent) TableName() string { return "mon_events" }

// MonStatsCurrent is one 5-minute aggregate as mon-server sent it. The bucket
// width lives in the row (BucketMs), not in the table name, so a later change
// of the probe interval is a data change rather than a schema change.
type MonStatsCurrent struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	MonClientId string `json:"monClientId" gorm:"size:64;uniqueIndex:idx_mon_current_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:8;uniqueIndex:idx_mon_current_key,priority:2;index:idx_mon_current_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"uniqueIndex:idx_mon_current_key,priority:3;index:idx_mon_current_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:8;uniqueIndex:idx_mon_current_key,priority:4"`
	BucketStart int64  `json:"bucketStart" gorm:"uniqueIndex:idx_mon_current_key,priority:5;index:idx_mon_current_inbound,priority:3"`
	BucketMs    int64  `json:"bucketMs"`
	NOk         int    `json:"nOk"`
	NFail       int    `json:"nFail"`
	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// TableName pins the name the spec uses (gorm would pluralise it).
func (MonStatsCurrent) TableName() string { return "mon_stats_current" }

// MonStatsRollup is a coarser aggregate rebuilt from MonStatsCurrent in the
// same transaction that stores the fine rows. NBuckets counts the fine rows
// that went in, so a rollup of an hour that is still open is visibly partial.
// LatAvg is weighted by NOk; HandshakeMs is the maximum over the step.
type MonStatsRollup struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	MonClientId string `json:"monClientId" gorm:"size:64;uniqueIndex:idx_mon_rollup_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:8;uniqueIndex:idx_mon_rollup_key,priority:2;index:idx_mon_rollup_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"uniqueIndex:idx_mon_rollup_key,priority:3;index:idx_mon_rollup_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:8;uniqueIndex:idx_mon_rollup_key,priority:4"`
	StepMs      int64  `json:"stepMs" gorm:"uniqueIndex:idx_mon_rollup_key,priority:5;index:idx_mon_rollup_inbound,priority:3"`
	BucketStart int64  `json:"bucketStart" gorm:"uniqueIndex:idx_mon_rollup_key,priority:6;index:idx_mon_rollup_inbound,priority:4"`
	NBuckets    int    `json:"nBuckets"`
	NOk         int    `json:"nOk"`
	NFail       int    `json:"nFail"`
	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// TableName pins the name the spec uses.
func (MonStatsRollup) TableName() string { return "mon_stats_rollup" }
