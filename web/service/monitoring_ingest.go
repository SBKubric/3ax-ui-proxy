package service

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Ingestion side of the contract: POST /events and POST /stats
// (monitoring-contract.md §4.6–4.7, monitoring-panel.md §4.3). The panel
// stores what mon-server says, keeps mon_targets as the latest known state per
// target, folds 5-minute buckets into the rollup, and hands fresh transitions
// to the Telegram hook. It decides nothing about UP or DOWN itself.

// monBucketMs is the width of a current-stats bucket in v1.
const monBucketMs int64 = 300000

// MonEventIn is one element of the POST /events batch.
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

// MonStatIn is one element of the POST /stats batch: a 5-minute bucket.
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

// MonIgnored names one batch element the panel skipped and why. Events are
// named by id, stats by their key.
type MonIgnored struct {
	Id    string `json:"id,omitempty"`
	Key   string `json:"key,omitempty"`
	Error string `json:"error"`
}

// MonEventsResult is the body of a successful POST /events.
type MonEventsResult struct {
	Accepted   int          `json:"accepted"`
	Duplicates int          `json:"duplicates"`
	Ignored    []MonIgnored `json:"ignored"`
}

// MonStatsResult is the body of a successful POST /stats.
type MonStatsResult struct {
	Accepted int          `json:"accepted"`
	Ignored  []MonIgnored `json:"ignored"`
}

// Reasons an element lands in ignored.
const (
	MonIgnoredUnknownInbound   = "unknown_inbound"
	MonIgnoredRetentionExpired = "retention_expired"
)

// MonEventNotifier is the Telegram side of §6. ApplyEvents calls it after the
// transaction commits with every stored event that still needs a
// notification (notified=false, kind target or mon_client); it returns the
// ids it delivered, which are then marked notified. The bot registers itself
// through SetMonEventNotifier when it starts.
type MonEventNotifier interface {
	NotifyMonitoringEvents(events []model.MonEvent) []string
}

var monNotifier struct {
	sync.RWMutex
	n MonEventNotifier
}

// SetMonEventNotifier installs (or, with nil, removes) the Telegram hook.
func SetMonEventNotifier(n MonEventNotifier) {
	monNotifier.Lock()
	monNotifier.n = n
	monNotifier.Unlock()
}

func currentMonEventNotifier() MonEventNotifier {
	monNotifier.RLock()
	defer monNotifier.RUnlock()
	return monNotifier.n
}

// --- validation --------------------------------------------------------------

var monClientIdRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

var (
	monTargetStates    = map[string]bool{model.MonStateUp: true, model.MonStateDown: true, model.MonStateFlapping: true, model.MonStateUnknown: true, model.MonStatePaused: true}
	monClientStates    = map[string]bool{"ONLINE": true, "OFFLINE": true}
	monPanelStates     = map[string]bool{"PANEL_UP": true, "PANEL_DOWN": true}
	monInboundKinds    = map[string]bool{model.MonInboundKindXray: true, model.MonInboundKindAwg: true}
	monPaths           = map[string]bool{model.MonPathDirect: true, model.MonPathProxy: true}
	monTargetStateRank = map[string]int{model.MonStateDown: 0, model.MonStateFlapping: 1, model.MonStateUnknown: 2, model.MonStateUp: 3, model.MonStatePaused: 4}
)

func errInvalidBody(format string, args ...any) *MonError {
	return &MonError{400, "invalid_body", fmt.Sprintf(format, args...)}
}

func validateMonEvent(i int, e *MonEventIn) error {
	at := func(field, format string, args ...any) error {
		return errInvalidBody("events[%d].%s: %s", i, field, fmt.Sprintf(format, args...))
	}
	if len(e.Id) != 36 || strings.ToLower(e.Id) != e.Id {
		return at("id", "must be a lowercase 36-character uuid")
	}
	if _, err := uuid.Parse(e.Id); err != nil {
		return at("id", "not a uuid: %v", err)
	}
	if e.Ts <= 0 {
		return at("ts", "must be a positive millisecond timestamp")
	}
	states := map[string]bool(nil)
	switch e.Kind {
	case model.MonEventKindTarget:
		states = monTargetStates
		if !monClientIdRe.MatchString(e.MonClientId) {
			return at("monClientId", "required, up to 64 characters of [A-Za-z0-9_.-]")
		}
		if !monInboundKinds[e.InboundKind] {
			return at("inboundKind", "unknown value %q", e.InboundKind)
		}
		if e.InboundId < 0 {
			return at("inboundId", "must not be negative")
		}
		if !monPaths[e.Path] {
			return at("path", "unknown value %q", e.Path)
		}
	case model.MonEventKindMonClient:
		states = monClientStates
		if !monClientIdRe.MatchString(e.MonClientId) {
			return at("monClientId", "required, up to 64 characters of [A-Za-z0-9_.-]")
		}
	case model.MonEventKindPanel:
		states = monPanelStates
	default:
		return at("kind", "unknown value %q", e.Kind)
	}
	if !states[e.To] {
		return at("to", "unknown value %q for kind %s", e.To, e.Kind)
	}
	if e.From != "" && !states[e.From] {
		return at("from", "unknown value %q for kind %s", e.From, e.Kind)
	}
	if len(e.Reason) > 128 {
		return at("reason", "longer than 128 characters")
	}
	return nil
}

func validateMonStat(i int, s *MonStatIn) error {
	at := func(field, format string, args ...any) error {
		return errInvalidBody("stats[%d].%s: %s", i, field, fmt.Sprintf(format, args...))
	}
	if !monClientIdRe.MatchString(s.MonClientId) {
		return at("monClientId", "required, up to 64 characters of [A-Za-z0-9_.-]")
	}
	if !monInboundKinds[s.InboundKind] {
		return at("inboundKind", "unknown value %q", s.InboundKind)
	}
	if s.InboundId < 0 {
		return at("inboundId", "must not be negative")
	}
	if !monPaths[s.Path] {
		return at("path", "unknown value %q", s.Path)
	}
	if s.BucketStart <= 0 || s.BucketStart%monBucketMs != 0 {
		return at("bucketStart", "must be a positive multiple of %d ms", monBucketMs)
	}
	if s.NOk < 0 || s.NFail < 0 {
		return at("nOk", "counts must not be negative")
	}
	for name, v := range map[string]*int64{"latencyMinMs": s.LatencyMinMs, "latencyAvgMs": s.LatencyAvgMs, "latencyMaxMs": s.LatencyMaxMs, "handshakeMs": s.HandshakeMs} {
		if v != nil && *v < 0 {
			return at(name, "must not be negative")
		}
	}
	return nil
}

// knownInbounds is the set of monitoring keys the panel currently has.
func (s *MonitoringService) knownInbounds() (map[MonInboundRef]bool, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	known := make(map[MonInboundRef]bool, len(inbounds))
	for _, ib := range inbounds {
		known[MonInboundRef{ib.Kind, ib.InboundId}] = true
	}
	return known, nil
}

// --- events ------------------------------------------------------------------

// ApplyEvents is POST /events. The batch is validated whole before anything
// is written, then applied in ts order inside one transaction: each event
// goes into the feed once (a repeated id is a duplicate, not an error), a
// target event moves its mon_targets row forward unless it is older than the
// state already there, and events of unknown inbounds are skipped and named
// in ignored. After the commit the Telegram hook sees the stored events that
// still want a notification.
func (s *MonitoringService) ApplyEvents(batch []MonEventIn) (*MonEventsResult, error) {
	for i := range batch {
		if err := validateMonEvent(i, &batch[i]); err != nil {
			return nil, err
		}
	}
	known, err := s.knownInbounds()
	if err != nil {
		return nil, err
	}
	ordered := append([]MonEventIn(nil), batch...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Ts < ordered[j].Ts })

	res := &MonEventsResult{Ignored: []MonIgnored{}}
	var pending []model.MonEvent
	now := time.Now().UnixMilli()

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, in := range ordered {
			isTarget := in.Kind == model.MonEventKindTarget
			if isTarget && !known[MonInboundRef{in.InboundKind, in.InboundId}] {
				res.Ignored = append(res.Ignored, MonIgnored{Id: in.Id, Error: MonIgnoredUnknownInbound})
				continue
			}
			ev := model.MonEvent{
				Id: in.Id, Ts: in.Ts, ReceivedAt: now, Kind: in.Kind, MonClientId: in.MonClientId,
				From: in.From, To: in.To, Reason: in.Reason, Notified: in.Notified,
			}
			if isTarget {
				ev.InboundKind, ev.InboundId, ev.Path = in.InboundKind, in.InboundId, in.Path
			}
			if in.Kind == model.MonEventKindPanel {
				ev.Notified = true // mon-server already told the operator
			}
			ins := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ev)
			if ins.Error != nil {
				return ins.Error
			}
			if ins.RowsAffected == 0 {
				res.Duplicates++
				continue
			}
			res.Accepted++
			if isTarget {
				if err := applyTargetEvent(tx, &ev); err != nil {
					return err
				}
			}
			if !ev.Notified {
				pending = append(pending, ev)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if n := currentMonEventNotifier(); n != nil && len(pending) > 0 {
		s.markNotified(n.NotifyMonitoringEvents(pending))
	}
	return res, nil
}

// applyTargetEvent moves the target's row to the event's state, creating it
// on first sight. An event older than the state already recorded (a late
// arrival after PANEL_DOWN) stays in the feed only.
func applyTargetEvent(tx *gorm.DB, ev *model.MonEvent) error {
	var target model.MonTarget
	err := tx.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ?",
		ev.MonClientId, ev.InboundKind, ev.InboundId, ev.Path).First(&target).Error
	if err != nil && !database.IsNotFound(err) {
		return err
	}
	if database.IsNotFound(err) {
		return tx.Create(&model.MonTarget{
			MonClientId: ev.MonClientId, InboundKind: ev.InboundKind, InboundId: ev.InboundId, Path: ev.Path,
			State: ev.To, Since: ev.Ts, Reason: ev.Reason,
		}).Error
	}
	if ev.Ts < target.Since {
		return nil
	}
	return tx.Model(&target).Updates(map[string]any{"state": ev.To, "since": ev.Ts, "reason": ev.Reason}).Error
}

// markNotified records that the hook delivered these events.
func (s *MonitoringService) markNotified(ids []string) {
	if len(ids) == 0 {
		return
	}
	if err := database.GetDB().Model(&model.MonEvent{}).Where("id IN ?", ids).Update("notified", true).Error; err != nil {
		logger.Warning("monitoring: could not mark events notified:", err)
	}
}

// --- stats -------------------------------------------------------------------

// retentionCutoff is the oldest bucket start the panel still accepts.
func (s *MonitoringService) retentionCutoff(now int64) int64 {
	days, err := s.settingService.GetMonRetentionDays()
	if err != nil || days <= 0 {
		days = 7
	}
	return now - int64(days)*24*60*60*1000
}

func (s *MonitoringService) rollupStepMs() int64 {
	minutes, err := s.settingService.GetMonRollupStepMinutes()
	if err != nil || minutes <= 0 {
		minutes = 60
	}
	return int64(minutes) * 60 * 1000
}

type monStatKey struct {
	MonClientId string
	InboundKind string
	InboundId   int
	Path        string
	BucketStart int64
}

func (k monStatKey) String() string {
	return fmt.Sprintf("%s/%s/%d/%s/%d", k.MonClientId, k.InboundKind, k.InboundId, k.Path, k.BucketStart)
}

// UpsertStats is POST /stats. The batch is validated whole; buckets older
// than monRetentionDays or for unknown inbounds are named in ignored; the
// rest replace their current-stats row by key, and every rollup bucket they
// touch is recomputed from current rows in the same transaction.
func (s *MonitoringService) UpsertStats(batch []MonStatIn) (*MonStatsResult, error) {
	for i := range batch {
		if err := validateMonStat(i, &batch[i]); err != nil {
			return nil, err
		}
	}
	known, err := s.knownInbounds()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	cutoff := s.retentionCutoff(now)
	stepMs := s.rollupStepMs()

	res := &MonStatsResult{Ignored: []MonIgnored{}}
	touched := map[monStatKey]bool{} // rollup buckets to recompute, keyed by their own start

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		for _, in := range batch {
			key := monStatKey{in.MonClientId, in.InboundKind, in.InboundId, in.Path, in.BucketStart}
			switch {
			case !known[MonInboundRef{in.InboundKind, in.InboundId}]:
				res.Ignored = append(res.Ignored, MonIgnored{Key: key.String(), Error: MonIgnoredUnknownInbound})
				continue
			case in.BucketStart < cutoff:
				res.Ignored = append(res.Ignored, MonIgnored{Key: key.String(), Error: MonIgnoredRetentionExpired})
				continue
			}
			row := model.MonStatsCurrent{
				MonClientId: in.MonClientId, InboundKind: in.InboundKind, InboundId: in.InboundId, Path: in.Path,
				BucketStart: in.BucketStart, BucketMs: monBucketMs, NOk: in.NOk, NFail: in.NFail,
				LatMin: in.LatencyMinMs, LatAvg: in.LatencyAvgMs, LatMax: in.LatencyMaxMs, HandshakeMs: in.HandshakeMs,
			}
			if row.NOk == 0 {
				row.LatMin, row.LatAvg, row.LatMax = nil, nil, nil
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "mon_client_id"}, {Name: "inbound_kind"}, {Name: "inbound_id"}, {Name: "path"}, {Name: "bucket_start"}},
				DoUpdates: clause.AssignmentColumns([]string{"bucket_ms", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"}),
			}).Create(&row).Error; err != nil {
				return err
			}
			// A target is known from its first aggregate too (§2.1).
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.MonTarget{
				MonClientId: in.MonClientId, InboundKind: in.InboundKind, InboundId: in.InboundId, Path: in.Path,
				State: model.MonStateUnknown, Since: in.BucketStart,
			}).Error; err != nil {
				return err
			}
			res.Accepted++
			touched[monStatKey{in.MonClientId, in.InboundKind, in.InboundId, in.Path, in.BucketStart - in.BucketStart%stepMs}] = true
		}
		for k := range touched {
			if err := recomputeRollupBucket(tx, k, stepMs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// recomputeRollupBucket rebuilds one rollup row from the current-stats rows
// inside [k.BucketStart, k.BucketStart+stepMs). n_buckets says how many
// 5-minute rows went in (a partial hour shows as fewer than stepMs/bucketMs);
// lat_avg is weighted by n_ok, lat_min/lat_max/handshake_ms are the extremes,
// all NULL when no row had a successful probe. Also used by the hourly job
// when the step changes.
func recomputeRollupBucket(tx *gorm.DB, k monStatKey, stepMs int64) error {
	var rows []model.MonStatsCurrent
	if err := tx.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ? AND bucket_start >= ? AND bucket_start < ?",
		k.MonClientId, k.InboundKind, k.InboundId, k.Path, k.BucketStart, k.BucketStart+stepMs).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return tx.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ? AND step_ms = ? AND bucket_start = ?",
			k.MonClientId, k.InboundKind, k.InboundId, k.Path, stepMs, k.BucketStart).Delete(&model.MonStatsRollup{}).Error
	}
	agg := model.MonStatsRollup{
		MonClientId: k.MonClientId, InboundKind: k.InboundKind, InboundId: k.InboundId, Path: k.Path,
		StepMs: stepMs, BucketStart: k.BucketStart, NBuckets: len(rows),
	}
	var weighted, weight int64
	for _, r := range rows {
		agg.NOk += r.NOk
		agg.NFail += r.NFail
		if r.NOk > 0 {
			if r.LatAvg != nil {
				weighted += *r.LatAvg * int64(r.NOk)
				weight += int64(r.NOk)
			}
			if r.LatMin != nil && (agg.LatMin == nil || *r.LatMin < *agg.LatMin) {
				v := *r.LatMin
				agg.LatMin = &v
			}
			if r.LatMax != nil && (agg.LatMax == nil || *r.LatMax > *agg.LatMax) {
				v := *r.LatMax
				agg.LatMax = &v
			}
		}
		if r.HandshakeMs != nil && (agg.HandshakeMs == nil || *r.HandshakeMs > *agg.HandshakeMs) {
			v := *r.HandshakeMs
			agg.HandshakeMs = &v
		}
	}
	if weight > 0 {
		v := weighted / weight
		agg.LatAvg = &v
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "mon_client_id"}, {Name: "inbound_kind"}, {Name: "inbound_id"}, {Name: "path"}, {Name: "step_ms"}, {Name: "bucket_start"}},
		DoUpdates: clause.AssignmentColumns([]string{"n_buckets", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"}),
	}).Create(&agg).Error
}

// --- badge -------------------------------------------------------------------

// WorstLiveTargetState folds the targets of one inbound into the state its
// Health badge shows: the worst state among mon-clients of the current
// registry snapshot, DOWN > FLAPPING > UNKNOWN > UP > PAUSED. Empty when the
// inbound has no live target. The panel's own STALE is layered on top by the
// caller (monitoring-panel.md §5).
func (s *MonitoringService) WorstLiveTargetState(inboundKind string, inboundId int) (string, error) {
	snapshot := s.RegistrySnapshot()
	if len(snapshot) == 0 {
		return "", nil
	}
	ids := make([]string, 0, len(snapshot))
	for _, c := range snapshot {
		ids = append(ids, c.Id)
	}
	var states []string
	if err := database.GetDB().Model(&model.MonTarget{}).
		Where("inbound_kind = ? AND inbound_id = ? AND mon_client_id IN ?", inboundKind, inboundId, ids).
		Pluck("state", &states).Error; err != nil {
		return "", err
	}
	return worstOfStates(states), nil
}

// worstOfStates folds target states by monTargetStateRank — DOWN > FLAPPING >
// UNKNOWN > UP > PAUSED; a state the rank table does not know counts as
// UNKNOWN. "" when there are no states. Shared by WorstLiveTargetState and
// the Monitoring page's per-inbound fold so the two never drift.
func worstOfStates(states []string) string {
	worst, worstRank := "", len(monTargetStateRank)+1
	for _, st := range states {
		rank, ok := monTargetStateRank[st]
		if !ok {
			rank = monTargetStateRank[model.MonStateUnknown]
		}
		if rank < worstRank {
			worst, worstRank = st, rank
		}
	}
	return worst
}
