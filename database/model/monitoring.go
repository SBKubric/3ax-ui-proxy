package model

import "gorm.io/gorm"

// Monitoring tables. The panel is a passive receiver (ADR 0003): mon-server
// decides UP/DOWN, the panel only stores what it is told — the state of every
// target, the event feed, and the latency/availability aggregates the
// Monitoring page draws. Spec: docs/spec/monitoring-panel.md §2.1; wire form:
// docs/spec/monitoring-contract.md.
//
// All timestamps are int64 milliseconds UTC, as everywhere else in the panel.
// Every row is keyed on the inbound it describes as (inbound_kind, inbound_id):
// an xray inbound by its id, the AmneziaWG server as ("awg", 0) — so the
// cascade on inbound deletion (§3.6, DeleteMonitoringByInbound) reaches all
// four tables with one key.
//
// SQLite index names are global, so every index here carries the idx_mon_
// prefix and is listed in namedIndexes (database/db.go).

// Inbound kinds a target can point at.
const (
	MonInboundKindXray = "xray"
	MonInboundKindAwg  = "awg"
)

// Paths a target is probed over: straight to the real server, or through the
// proxy front.
const (
	MonPathDirect = "direct"
	MonPathProxy  = "proxy"
)

// Target states as reported by mon-server. STALE is not a stored state: the
// panel derives it from monLastContact when mon-server itself goes quiet.
const (
	MonStateUp       = "UP"
	MonStateDown     = "DOWN"
	MonStateFlapping = "FLAPPING"
	MonStateUnknown  = "UNKNOWN"
	MonStatePaused   = "PAUSED"
)

// Event kinds in the feed.
const (
	MonEventKindTarget    = "target"     // a target changed state
	MonEventKindMonClient = "mon_client" // a mon-client went ONLINE/OFFLINE
	MonEventKindPanel     = "panel"      // mon-server lost/regained the panel
)

// MonTarget is the current state of one (mon-client, inbound, path) probe. A
// row appears with the first event or aggregate that carries its key and is
// removed when its mon-client leaves the registry snapshot or the inbound is
// deleted.
type MonTarget struct {
	Id int `json:"id" gorm:"primaryKey;autoIncrement"`

	MonClientId string `json:"monClientId" gorm:"size:64;not null;uniqueIndex:idx_mon_targets_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:16;not null;uniqueIndex:idx_mon_targets_key,priority:2;index:idx_mon_targets_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"not null;uniqueIndex:idx_mon_targets_key,priority:3;index:idx_mon_targets_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:16;not null;uniqueIndex:idx_mon_targets_key,priority:4"`

	State  string `json:"state" gorm:"size:16;not null"` // one of the MonState* constants
	Since  int64  `json:"since"`                         // when the current state began, ms
	Reason string `json:"reason"`                        // snake_case diagnosis behind the current state
	// UpdatedAt is kept by gorm in milliseconds, matching every other time here.
	UpdatedAt int64 `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

// MonEvent is one entry of the event feed. Id is a UUID v7 minted by
// mon-server, and is the deduplication key for POST /events.
//
// The wire fields are "from" and "to"; the columns are from_state / to_state
// because both words are SQL keywords and a bare `from` column would have to
// be quoted in every raw query.
type MonEvent struct {
	Id string `json:"id" gorm:"primaryKey;size:36"`

	Ts         int64 `json:"ts" gorm:"not null;index:idx_mon_events_ts;index:idx_mon_events_inbound_ts,priority:3"`
	ReceivedAt int64 `json:"receivedAt" gorm:"not null"`

	Kind string `json:"kind" gorm:"size:16;not null"` // one of the MonEventKind* constants

	// Set for kind=target and kind=mon_client; empty for kind=panel.
	MonClientId string `json:"monClientId" gorm:"size:64"`
	// Set for kind=target only; a panel or mon_client event carries zero values.
	InboundKind string `json:"inboundKind" gorm:"size:16;index:idx_mon_events_inbound_ts,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"index:idx_mon_events_inbound_ts,priority:2"`
	Path        string `json:"path" gorm:"size:16"`

	From   string `json:"from" gorm:"column:from_state;size:16"`
	To     string `json:"to" gorm:"column:to_state;size:16"`
	Reason string `json:"reason"`

	// Notified is true once the Telegram message for this transition went out,
	// or when mon-server said the panel need not send one.
	Notified bool `json:"notified"`
}

// MonStatsCurrent is one fine-grained bucket (bucket_ms wide, 5 minutes in v1)
// of probe results for one target. Rows come straight from POST /stats and are
// replaced on the same key.
//
// lat_* are integer milliseconds and NULL when n_ok = 0; handshake_ms is set for
// AmneziaWG targets only.
type MonStatsCurrent struct {
	Id int `json:"id" gorm:"primaryKey;autoIncrement"`

	MonClientId string `json:"monClientId" gorm:"size:64;not null;uniqueIndex:idx_mon_stats_current_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:16;not null;uniqueIndex:idx_mon_stats_current_key,priority:2;index:idx_mon_stats_current_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"not null;uniqueIndex:idx_mon_stats_current_key,priority:3;index:idx_mon_stats_current_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:16;not null;uniqueIndex:idx_mon_stats_current_key,priority:4"`
	BucketStart int64  `json:"bucketStart" gorm:"not null;uniqueIndex:idx_mon_stats_current_key,priority:5;index:idx_mon_stats_current_inbound,priority:3"`
	BucketMs    int64  `json:"bucketMs" gorm:"not null;default:300000"`

	NOk   int `json:"nOk"`
	NFail int `json:"nFail"`

	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// TableName pins the name: gorm would otherwise pluralise it to
// mon_stats_currents.
func (MonStatsCurrent) TableName() string { return "mon_stats_current" }

// MonStatsRollup is a coarser bucket (step_ms wide) built from n_buckets rows of
// MonStatsCurrent, so the week-long views stay cheap. lat_avg is weighted by
// n_ok across the folded buckets; handshake_ms is their maximum.
type MonStatsRollup struct {
	Id int `json:"id" gorm:"primaryKey;autoIncrement"`

	MonClientId string `json:"monClientId" gorm:"size:64;not null;uniqueIndex:idx_mon_stats_rollup_key,priority:1"`
	InboundKind string `json:"inboundKind" gorm:"size:16;not null;uniqueIndex:idx_mon_stats_rollup_key,priority:2;index:idx_mon_stats_rollup_inbound,priority:1"`
	InboundId   int    `json:"inboundId" gorm:"not null;uniqueIndex:idx_mon_stats_rollup_key,priority:3;index:idx_mon_stats_rollup_inbound,priority:2"`
	Path        string `json:"path" gorm:"size:16;not null;uniqueIndex:idx_mon_stats_rollup_key,priority:4"`
	StepMs      int64  `json:"stepMs" gorm:"not null;uniqueIndex:idx_mon_stats_rollup_key,priority:5;index:idx_mon_stats_rollup_inbound,priority:3"`
	BucketStart int64  `json:"bucketStart" gorm:"not null;uniqueIndex:idx_mon_stats_rollup_key,priority:6;index:idx_mon_stats_rollup_inbound,priority:4"`

	NBuckets int `json:"nBuckets"`
	NOk      int `json:"nOk"`
	NFail    int `json:"nFail"`

	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// TableName pins the name: gorm would otherwise pluralise it to
// mon_stats_rollups.
func (MonStatsRollup) TableName() string { return "mon_stats_rollup" }

// DeleteMonitoringByInbound drops every monitoring row that describes one
// inbound — its targets, events and both aggregate tables — the way upstream
// drops client_traffics when an inbound goes. Call it on the transaction that
// deletes the inbound so the panel never keeps health for something that no
// longer exists (§3.6). Rows of other inbounds are untouched; a mon_client or
// panel event carries no inbound and stays.
func DeleteMonitoringByInbound(tx *gorm.DB, inboundKind string, inboundId int) error {
	where := "inbound_kind = ? AND inbound_id = ?"
	for _, m := range []any{
		&MonTarget{},
		&MonEvent{},
		&MonStatsCurrent{},
		&MonStatsRollup{},
	} {
		if err := tx.Where(where, inboundKind, inboundId).Delete(m).Error; err != nil {
			return err
		}
	}
	return nil
}
