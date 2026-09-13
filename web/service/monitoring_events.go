package service

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// POST /events and POST /stats (docs/spec/monitoring-panel.md §4.3,
// contract §4.6–4.7). Both validate the whole batch before touching the
// database — a 4xx makes mon-server drop the batch, so a half-applied one
// must never happen — and apply it in one transaction.

// Batch limits of contract §3; over them the answer is 413 batch_too_large.
const (
	MonMaxEvents     = 1000
	MonMaxStats      = 2000
	MonMaxMonClients = 200
)

// MonEventIn is one event as mon-server sends it.
type MonEventIn struct {
	Id          string `json:"id"`
	Ts          int64  `json:"ts"`
	Kind        string `json:"kind"`
	MonClientId string `json:"monClientId"`
	InboundKind string `json:"inboundKind"`
	InboundId   int    `json:"inboundId"`
	Path        string `json:"path"`
	From        string `json:"from"`
	To          string `json:"to"`
	Reason      string `json:"reason"`
	Notified    bool   `json:"notified"`
}

// MonEventsBatch is the body of POST /events.
type MonEventsBatch struct {
	Events []MonEventIn `json:"events"`
}

// MonIgnoredEvent names an event the panel skipped and why.
type MonIgnoredEvent struct {
	Id    string `json:"id"`
	Error string `json:"error"`
}

// MonEventsResult is the body of a 200 to POST /events.
type MonEventsResult struct {
	Accepted   int               `json:"accepted"`
	Duplicates int               `json:"duplicates"`
	Ignored    []MonIgnoredEvent `json:"ignored"`
}

// MonStatIn is one 5-minute aggregate as mon-server sends it.
type MonStatIn struct {
	MonClientId  string `json:"monClientId"`
	InboundKind  string `json:"inboundKind"`
	InboundId    int    `json:"inboundId"`
	Path         string `json:"path"`
	BucketStart  int64  `json:"bucketStart"`
	NOk          int    `json:"nOk"`
	NFail        int    `json:"nFail"`
	LatencyMinMs *int64 `json:"latencyMinMs"`
	LatencyAvgMs *int64 `json:"latencyAvgMs"`
	LatencyMaxMs *int64 `json:"latencyMaxMs"`
	HandshakeMs  *int64 `json:"handshakeMs"`
}

// MonStatsBatch is the body of POST /stats.
type MonStatsBatch struct {
	Stats []MonStatIn `json:"stats"`
}

// MonIgnoredStat names an aggregate the panel skipped and why.
type MonIgnoredStat struct {
	MonClientId string `json:"monClientId"`
	InboundKind string `json:"inboundKind"`
	InboundId   int    `json:"inboundId"`
	Path        string `json:"path"`
	BucketStart int64  `json:"bucketStart"`
	Error       string `json:"error"`
}

// MonStatsResult is the body of a 200 to POST /stats.
type MonStatsResult struct {
	Accepted int              `json:"accepted"`
	Ignored  []MonIgnoredStat `json:"ignored"`
}

// MonNotification is what the Telegram side receives for an event the panel
// has to announce: the stored event plus the context the message needs.
type MonNotification struct {
	Event         model.MonEvent
	InboundRemark string
	MonClient     MonClient
	// PrevState and PrevSince describe the target before this event, so an
	// UP can say how long it was down.
	PrevState string
	PrevSince int64
}

// MonEventNotifier receives the events of a batch that carried
// notified=false, after the batch is committed. Installed by the Telegram
// side; nil means nobody listens.
type MonEventNotifier func([]MonNotification)

var monEventNotifier MonEventNotifier

// SetMonEventNotifier installs the notifier.
func SetMonEventNotifier(n MonEventNotifier) {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	monEventNotifier = n
}

func currentMonEventNotifier() MonEventNotifier {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	return monEventNotifier
}

// Error codes of contract §3 used here.
const (
	MonErrInvalidBody    = "invalid_body"
	MonErrBatchTooLarge  = "batch_too_large"
	MonErrUnknownInbound = "unknown_inbound"
	MonErrTooOld         = "too_old"
)

var (
	monClientIdRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	monTargetState = map[string]bool{model.MonStateUp: true, model.MonStateDown: true, model.MonStateFlapping: true, model.MonStateUnknown: true, model.MonStatePaused: true}
	monClientState = map[string]bool{model.MonClientOnline: true, model.MonClientOffline: true}
	monPanelState  = map[string]bool{"PANEL_UP": true, "PANEL_DOWN": true}
)

func invalidBody(format string, args ...any) *MonError {
	return &MonError{Status: http.StatusBadRequest, Code: MonErrInvalidBody, Message: fmt.Sprintf(format, args...)}
}

func batchTooLarge(what string, n, limit int) *MonError {
	return &MonError{Status: http.StatusRequestEntityTooLarge, Code: MonErrBatchTooLarge,
		Message: fmt.Sprintf("%s: %d elements, the limit is %d", what, n, limit)}
}

// ValidMonClientId reports whether id has the shape contract §3 gives a
// mon-client id: 1–64 characters of [A-Za-z0-9_.-].
func ValidMonClientId(id string) bool { return monClientIdRe.MatchString(id) }

func validKind(kind string) bool { return kind == model.MonKindXray || kind == model.MonKindAwg }
func validPath(path string) bool { return path == model.MonPathDirect || path == model.MonPathProxy }

// validateEvents checks a whole batch against contract §4.6.
func validateEvents(batch *MonEventsBatch) error {
	if len(batch.Events) > MonMaxEvents {
		return batchTooLarge("events", len(batch.Events), MonMaxEvents)
	}
	for i := range batch.Events {
		e := &batch.Events[i]
		at := fmt.Sprintf("events[%d]", i)
		if len(e.Id) != 36 || strings.ToLower(e.Id) != e.Id {
			return invalidBody("%s.id: not a lowercase 36-character uuid", at)
		}
		if _, err := uuid.Parse(e.Id); err != nil {
			return invalidBody("%s.id: %v", at, err)
		}
		if e.Ts <= 0 {
			return invalidBody("%s.ts: must be a positive epoch in milliseconds", at)
		}
		switch e.Kind {
		case model.MonEventTarget:
			if !ValidMonClientId(e.MonClientId) {
				return invalidBody("%s.monClientId: required, at most 64 of [A-Za-z0-9_.-]", at)
			}
			if !validKind(e.InboundKind) {
				return invalidBody("%s.inboundKind: unknown value %q", at, e.InboundKind)
			}
			if e.InboundId < 0 || (e.InboundKind == model.MonKindAwg && e.InboundId != 0) {
				return invalidBody("%s.inboundId: invalid for kind %s", at, e.InboundKind)
			}
			if !validPath(e.Path) {
				return invalidBody("%s.path: unknown value %q", at, e.Path)
			}
			if !monTargetState[e.To] {
				return invalidBody("%s.to: unknown value %q", at, e.To)
			}
			if e.From != "" && !monTargetState[e.From] {
				return invalidBody("%s.from: unknown value %q", at, e.From)
			}
		case model.MonEventMonClient:
			if !ValidMonClientId(e.MonClientId) {
				return invalidBody("%s.monClientId: required, at most 64 of [A-Za-z0-9_.-]", at)
			}
			if !monClientState[e.To] || (e.From != "" && !monClientState[e.From]) {
				return invalidBody("%s: mon_client transition %q -> %q is not ONLINE/OFFLINE", at, e.From, e.To)
			}
		case model.MonEventPanel:
			if !monPanelState[e.To] || (e.From != "" && !monPanelState[e.From]) {
				return invalidBody("%s: panel transition %q -> %q is not PANEL_UP/PANEL_DOWN", at, e.From, e.To)
			}
		default:
			return invalidBody("%s.kind: unknown value %q", at, e.Kind)
		}
	}
	return nil
}

// validateStats checks a whole batch against contract §4.7.
func validateStats(batch *MonStatsBatch) error {
	if len(batch.Stats) > MonMaxStats {
		return batchTooLarge("stats", len(batch.Stats), MonMaxStats)
	}
	for i := range batch.Stats {
		s := &batch.Stats[i]
		at := fmt.Sprintf("stats[%d]", i)
		if !ValidMonClientId(s.MonClientId) {
			return invalidBody("%s.monClientId: required, at most 64 of [A-Za-z0-9_.-]", at)
		}
		if !validKind(s.InboundKind) {
			return invalidBody("%s.inboundKind: unknown value %q", at, s.InboundKind)
		}
		if s.InboundId < 0 || (s.InboundKind == model.MonKindAwg && s.InboundId != 0) {
			return invalidBody("%s.inboundId: invalid for kind %s", at, s.InboundKind)
		}
		if !validPath(s.Path) {
			return invalidBody("%s.path: unknown value %q", at, s.Path)
		}
		if s.BucketStart <= 0 || s.BucketStart%model.MonBucketMs != 0 {
			return invalidBody("%s.bucketStart: must be a positive multiple of %d", at, model.MonBucketMs)
		}
		if s.NOk < 0 || s.NFail < 0 {
			return invalidBody("%s: nOk and nFail must not be negative", at)
		}
		for name, v := range map[string]*int64{"latencyMinMs": s.LatencyMinMs, "latencyAvgMs": s.LatencyAvgMs, "latencyMaxMs": s.LatencyMaxMs, "handshakeMs": s.HandshakeMs} {
			if v != nil && *v < 0 {
				return invalidBody("%s.%s: must not be negative", at, name)
			}
		}
	}
	return nil
}

// knownInbounds is the set of (kind, id) the panel has, for the ignored rule.
func (s *MonitoringService) knownInbounds() (map[MonInboundRef]string, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	known := make(map[MonInboundRef]string, len(inbounds))
	for _, ib := range inbounds {
		known[MonInboundRef{Kind: ib.Kind, InboundId: ib.InboundId}] = ib.Remark
	}
	return known, nil
}

// ApplyEvents is POST /events. In one transaction, in ts order: every event
// goes to the feed unless its id is already there (a duplicate, not an
// error) or its inbound is unknown (ignored); a target event then moves its
// target's state unless it is older than what the target already shows.
// mon_client and panel events touch no target. After the commit the events
// that carried notified=false, panel events excepted, go to the notifier.
func (s *MonitoringService) ApplyEvents(batch *MonEventsBatch) (*MonEventsResult, error) {
	if err := validateEvents(batch); err != nil {
		return nil, err
	}
	known, err := s.knownInbounds()
	if err != nil {
		return nil, err
	}
	snapshot := s.Snapshot()
	monClients := make(map[string]MonClient, len(snapshot))
	for _, c := range snapshot {
		monClients[c.Id] = c
	}

	events := append([]MonEventIn(nil), batch.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts < events[j].Ts })

	result := &MonEventsResult{Ignored: []MonIgnoredEvent{}}
	var notifications []MonNotification
	now := time.Now().UnixMilli()

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, e := range events {
			ref := MonInboundRef{Kind: e.InboundKind, InboundId: e.InboundId}
			remark, isKnown := known[ref]
			if e.Kind == model.MonEventTarget && !isKnown {
				result.Ignored = append(result.Ignored, MonIgnoredEvent{Id: e.Id, Error: MonErrUnknownInbound})
				continue
			}
			row := model.MonEvent{
				Id: e.Id, Ts: e.Ts, ReceivedAt: now, Kind: e.Kind, MonClientId: e.MonClientId,
				FromState: e.From, ToState: e.To, Reason: e.Reason, Notified: e.Notified,
			}
			if e.Kind == model.MonEventTarget {
				row.InboundKind, row.InboundId, row.Path = e.InboundKind, e.InboundId, e.Path
			}
			if e.Kind == model.MonEventPanel {
				row.Notified = true // mon-server sent it; the feed only records it
			}
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				result.Duplicates++
				continue
			}
			result.Accepted++

			note := MonNotification{Event: row, InboundRemark: remark, MonClient: monClients[e.MonClientId]}
			if note.MonClient.Id == "" {
				note.MonClient.Id = e.MonClientId
			}
			if e.Kind == model.MonEventTarget {
				prevState, prevSince, err := applyTargetEvent(tx, &e, now)
				if err != nil {
					return err
				}
				note.PrevState, note.PrevSince = prevState, prevSince
			}
			if !row.Notified {
				notifications = append(notifications, note)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if notify := currentMonEventNotifier(); notify != nil && len(notifications) > 0 {
		notify(notifications)
	}
	return result, nil
}

// applyTargetEvent moves the target row of e to e.To unless the row already
// shows something newer. It returns the state and since the row had before.
func applyTargetEvent(tx *gorm.DB, e *MonEventIn, now int64) (prevState string, prevSince int64, err error) {
	var target model.MonTarget
	err = tx.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ?",
		e.MonClientId, e.InboundKind, e.InboundId, e.Path).First(&target).Error
	switch {
	case database.IsNotFound(err):
		target = model.MonTarget{MonClientId: e.MonClientId, InboundKind: e.InboundKind, InboundId: e.InboundId, Path: e.Path,
			State: e.To, Since: e.Ts, Reason: e.Reason, UpdatedAt: now}
		return "", 0, tx.Create(&target).Error
	case err != nil:
		return "", 0, err
	}
	prevState, prevSince = target.State, target.Since
	if e.Ts < target.Since {
		return prevState, prevSince, nil // late arrival: in the feed, not in the state
	}
	return prevState, prevSince, tx.Model(&target).Updates(map[string]any{
		"state": e.To, "since": e.Ts, "reason": e.Reason, "updated_at": now,
	}).Error
}

// retentionFloor is the oldest millisecond the fine tables keep.
func (s *MonitoringService) retentionFloor(now time.Time) (int64, error) {
	days, err := s.settingService.GetMonRetentionDays()
	if err != nil {
		return 0, err
	}
	if days <= 0 {
		days = 7
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli(), nil
}

// rollupStepMs is the rollup step from the settings, in milliseconds.
func (s *MonitoringService) rollupStepMs() (int64, error) {
	minutes, err := s.settingService.GetMonRollupStepMinutes()
	if err != nil {
		return 0, err
	}
	if minutes <= 0 {
		minutes = 60
	}
	return int64(minutes) * 60_000, nil
}

// monSeriesKey identifies one series: a mon-client probing one inbound over
// one path.
type monSeriesKey struct {
	MonClientId string
	InboundKind string
	InboundId   int
	Path        string
}

// UpsertStats is POST /stats: upsert by key into mon_stats_current, then, in
// the same transaction, recompute every rollup bucket a row landed in and
// upsert it with the current step. Buckets of unknown inbounds and buckets
// older than the retention are ignored, not stored.
func (s *MonitoringService) UpsertStats(batch *MonStatsBatch) (*MonStatsResult, error) {
	if err := validateStats(batch); err != nil {
		return nil, err
	}
	known, err := s.knownInbounds()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	floor, err := s.retentionFloor(now)
	if err != nil {
		return nil, err
	}
	stepMs, err := s.rollupStepMs()
	if err != nil {
		return nil, err
	}

	result := &MonStatsResult{Ignored: []MonIgnoredStat{}}
	type rollupKey struct {
		monSeriesKey
		bucketStart int64
	}
	affected := map[rollupKey]struct{}{}

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, in := range batch.Stats {
			ignored := func(code string) {
				result.Ignored = append(result.Ignored, MonIgnoredStat{MonClientId: in.MonClientId, InboundKind: in.InboundKind,
					InboundId: in.InboundId, Path: in.Path, BucketStart: in.BucketStart, Error: code})
			}
			if _, ok := known[MonInboundRef{Kind: in.InboundKind, InboundId: in.InboundId}]; !ok {
				ignored(MonErrUnknownInbound)
				continue
			}
			if in.BucketStart < floor {
				ignored(MonErrTooOld)
				continue
			}
			row := model.MonStatsCurrent{
				MonClientId: in.MonClientId, InboundKind: in.InboundKind, InboundId: in.InboundId, Path: in.Path,
				BucketStart: in.BucketStart, BucketMs: model.MonBucketMs, NOk: in.NOk, NFail: in.NFail,
				LatMin: in.LatencyMinMs, LatAvg: in.LatencyAvgMs, LatMax: in.LatencyMaxMs, HandshakeMs: in.HandshakeMs,
			}
			if row.NOk == 0 {
				row.LatMin, row.LatAvg, row.LatMax = nil, nil, nil // no successful probe, no latency
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "mon_client_id"}, {Name: "inbound_kind"}, {Name: "inbound_id"}, {Name: "path"}, {Name: "bucket_start"}},
				DoUpdates: clause.AssignmentColumns([]string{"bucket_ms", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"}),
			}).Create(&row).Error; err != nil {
				return err
			}
			key := monSeriesKey{in.MonClientId, in.InboundKind, in.InboundId, in.Path}
			if err := ensureTargetRow(tx, key, now.UnixMilli()); err != nil {
				return err
			}
			result.Accepted++
			affected[rollupKey{key, in.BucketStart - in.BucketStart%stepMs}] = struct{}{}
		}
		for k := range affected {
			if err := rebuildRollupBucket(tx, k.monSeriesKey, stepMs, k.bucketStart); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ensureTargetRow creates the target row of a series the first aggregate
// arrives for, in UNKNOWN with no since: the state, and its time, come with
// the first event, which must not read as older than this placeholder.
func ensureTargetRow(tx *gorm.DB, key monSeriesKey, now int64) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.MonTarget{
		MonClientId: key.MonClientId, InboundKind: key.InboundKind, InboundId: key.InboundId, Path: key.Path,
		State: model.MonStateUnknown, Since: 0, UpdatedAt: now,
	}).Error
}

// rebuildRollupBucket recomputes one rollup row from the current rows in
// [bucketStart, bucketStart+stepMs): counts summed, lat_min/lat_max the
// extremes, lat_avg weighted by n_ok over the rows that have one,
// handshake_ms the maximum, n_buckets the number of rows that went in. A
// bucket with no current rows left loses its rollup row.
func rebuildRollupBucket(tx *gorm.DB, key monSeriesKey, stepMs, bucketStart int64) error {
	var agg struct {
		NBuckets  int
		NOk       int
		NFail     int
		LatMin    *int64
		LatMax    *int64
		LatSum    *float64
		LatWeight *int64
		Handshake *int64
	}
	err := tx.Raw(`SELECT COUNT(*) AS n_buckets, COALESCE(SUM(n_ok), 0) AS n_ok, COALESCE(SUM(n_fail), 0) AS n_fail,
		MIN(lat_min) AS lat_min, MAX(lat_max) AS lat_max,
		SUM(CASE WHEN lat_avg IS NOT NULL THEN lat_avg * n_ok END) AS lat_sum,
		SUM(CASE WHEN lat_avg IS NOT NULL THEN n_ok END) AS lat_weight,
		MAX(handshake_ms) AS handshake
		FROM mon_stats_current
		WHERE mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ? AND bucket_start >= ? AND bucket_start < ?`,
		key.MonClientId, key.InboundKind, key.InboundId, key.Path, bucketStart, bucketStart+stepMs).Scan(&agg).Error
	if err != nil {
		return err
	}
	where := tx.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ? AND step_ms = ? AND bucket_start = ?",
		key.MonClientId, key.InboundKind, key.InboundId, key.Path, stepMs, bucketStart)
	if agg.NBuckets == 0 {
		return where.Delete(&model.MonStatsRollup{}).Error
	}
	var latAvg *int64
	if agg.LatSum != nil && agg.LatWeight != nil && *agg.LatWeight > 0 {
		v := int64(*agg.LatSum / float64(*agg.LatWeight))
		latAvg = &v
	}
	row := model.MonStatsRollup{
		MonClientId: key.MonClientId, InboundKind: key.InboundKind, InboundId: key.InboundId, Path: key.Path,
		StepMs: stepMs, BucketStart: bucketStart, NBuckets: agg.NBuckets, NOk: agg.NOk, NFail: agg.NFail,
		LatMin: agg.LatMin, LatAvg: latAvg, LatMax: agg.LatMax, HandshakeMs: agg.Handshake,
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "mon_client_id"}, {Name: "inbound_kind"}, {Name: "inbound_id"}, {Name: "path"}, {Name: "step_ms"}, {Name: "bucket_start"}},
		DoUpdates: clause.AssignmentColumns([]string{"n_buckets", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"}),
	}).Create(&row).Error
}

// RebuildRollup recomputes every rollup bucket of the given step from the
// current rows newer than floor. The hourly job calls it when the step
// setting changes; rows of another step stay until their own retention.
func (s *MonitoringService) RebuildRollup(stepMs, floor int64) error {
	type bucket struct {
		MonClientId string
		InboundKind string
		InboundId   int
		Path        string
		BucketStart int64
	}
	var buckets []bucket
	db := database.GetDB()
	if err := db.Raw(`SELECT DISTINCT mon_client_id, inbound_kind, inbound_id, path, bucket_start - (bucket_start % ?) AS bucket_start
		FROM mon_stats_current WHERE bucket_start >= ?`, stepMs, floor).Scan(&buckets).Error; err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, b := range buckets {
			if err := rebuildRollupBucket(tx, monSeriesKey{b.MonClientId, b.InboundKind, b.InboundId, b.Path}, stepMs, b.BucketStart); err != nil {
				return err
			}
		}
		return nil
	})
}

// monStatePriority orders target states worst first for the Health badge.
var monStatePriority = map[string]int{
	model.MonStateDown: 0, model.MonStateFlapping: 1, model.MonStateUnknown: 2, model.MonStateUp: 3, model.MonStatePaused: 4,
}

// worstState picks the worst of two states by monStatePriority; an empty
// state loses to anything.
func worstState(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	pa, okA := monStatePriority[a]
	pb, okB := monStatePriority[b]
	if !okA {
		return b
	}
	if !okB || pa <= pb {
		return a
	}
	return b
}

// WorstLiveTargetStates folds the target rows into one state per inbound,
// DOWN > FLAPPING > UNKNOWN > UP > PAUSED, counting only mon-clients that are
// in the current registry snapshot. The panel's STALE flag, when raised,
// overrides everything and is the caller's to apply.
func (s *MonitoringService) WorstLiveTargetStates() (map[MonInboundRef]string, error) {
	live := s.SnapshotIds()
	var targets []model.MonTarget
	if err := database.GetDB().Find(&targets).Error; err != nil {
		return nil, err
	}
	out := map[MonInboundRef]string{}
	for _, t := range targets {
		if _, ok := live[t.MonClientId]; !ok {
			continue
		}
		ref := MonInboundRef{Kind: t.InboundKind, InboundId: t.InboundId}
		out[ref] = worstState(out[ref], t.State)
	}
	return out, nil
}

// WorstLiveTargetState is WorstLiveTargetStates for one inbound; empty when
// no live mon-client has a target on it.
func (s *MonitoringService) WorstLiveTargetState(kind string, inboundId int) string {
	states, err := s.WorstLiveTargetStates()
	if err != nil {
		logger.Warning("monitoring: could not read target states:", err)
		return ""
	}
	return states[MonInboundRef{Kind: kind, InboundId: inboundId}]
}
