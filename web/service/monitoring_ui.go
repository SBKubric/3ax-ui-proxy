package service

import (
	"math"
	"sort"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// Everything the Monitoring page reads (monitoring-panel.md §7.1 and §7.4).
// The four session handlers of web/controller/monitoring_ui.go are thin: each
// one parses its query string and calls exactly one method here, so the page,
// the tests and any later reader of the same numbers share one implementation.
//
// GET summary is not in this file at all — it is Summary() from
// monitoring_summary.go, the same calculation the daily Telegram digest
// prints (§6), reused unchanged.
//
// Times are int64 milliseconds UTC everywhere; the browser renders them in
// its own zone.

// monUIWindow24 is the window the targets view reports uptime and coverage
// over, fixed by §7.1 ("uptime 24h").
const monUIWindow24 = 24 * time.Hour

// Sources a stats answer can come from, so the page can say which table it is
// looking at.
const (
	MonUISourceCurrent = "current"
	MonUISourceRollup  = "rollup"
)

// monUIEventsDefaultLimit and monUIEventsMaxLimit bound the feed (§7.4:
// limit ≤ 200).
const (
	monUIEventsDefaultLimit = 50
	monUIEventsMaxLimit     = 200
)

// --- GET targets -------------------------------------------------------------

// MonUITarget is one row of an inbound's target table: the state mon-server
// reported, enriched with the mon-client it came from and with the 24h
// availability the page prints beside it.
//
// Uptime24 and LatAvg24 are nil when nothing was measured in the window,
// which is not the same as 0 % and not the same as 0 ms.
type MonUITarget struct {
	MonClientId   string `json:"monClientId"`
	MonClientName string `json:"monClientName"`
	Region        string `json:"region"`
	ClientState   string `json:"clientState"` // ONLINE/OFFLINE as the registry snapshot has it

	InboundKind string `json:"inboundKind"`
	InboundId   int    `json:"inboundId"`
	Path        string `json:"path"`

	State  string `json:"state"`
	Since  int64  `json:"since"`
	Reason string `json:"reason"`

	Uptime24   *float64 `json:"uptime24"`
	Coverage24 float64  `json:"coverage24"`
	LatAvg24   *int64   `json:"latAvg24"`

	// Retired: the mon-client is no longer in the registry snapshot (§4.4).
	// Such a target keeps its last known state but is left out of the Health
	// fold and marked on the page.
	Retired bool `json:"retired"`
}

// MonUIInbound is one card of the page: the sanitised inbound, the state its
// Health badge shows, and its targets sorted by (monClientId, path).
type MonUIInbound struct {
	MonInbound
	// Worst is the badge state over live targets only, "" when the inbound
	// has none (§7.2).
	Worst   string        `json:"worst"`
	Targets []MonUITarget `json:"targets"`
}

// MonUITargets is the whole GET targets answer: the page draws its header
// from the top-level fields, its mon-client pills from MonClients and one
// card per inbound.
type MonUITargets struct {
	Now         int64       `json:"now"`
	LastContact int64       `json:"lastContact"`
	Stale       bool        `json:"stale"`
	StaleSince  int64       `json:"staleSince"`
	Override    MonOverride `json:"override"`
	MonClients  []MonClient `json:"monClients"`
	// Inbounds carries every entry of Inbounds(), disabled ones included, in
	// that order: a paused inbound is still a card on the page.
	Inbounds []MonUIInbound `json:"inbounds"`
}

// monUITargetKey identifies one target row across the two queries below.
type monUITargetKey struct {
	MonClientId string
	InboundKind string
	InboundId   int
	Path        string
}

// monUITargetAgg is one GROUP BY row of the 24h window: the sums behind
// uptime, the bucket count behind coverage, and the n_ok-weighted latency.
type monUITargetAgg struct {
	MonClientId string
	InboundKind string
	InboundId   int
	Path        string
	NOk         int
	NFail       int
	Received    int
	BucketMs    int64
	LatWeighted int64
	LatWeight   int64
}

// UITargets is GET targets (§7.4): the live mon_targets rows folded into the
// inbounds they belong to, each enriched from the registry snapshot and from
// mon_stats_current over [now−24h, now). Two queries, joined in Go — the
// per-inbound badge is folded from the same rows rather than asked of the
// database once per inbound.
func (s *MonitoringService) UITargets(now time.Time) (*MonUITargets, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	clients := s.RegistrySnapshot()
	if clients == nil {
		// An empty registry has to reach the page as [], not null: the page
		// counts the mon-clients before it draws them, and null has no length.
		clients = []MonClient{}
	}
	byId := make(map[string]MonClient, len(clients))
	for _, c := range clients {
		byId[c.Id] = c
	}

	var rows []model.MonTarget
	if err := database.GetDB().Find(&rows).Error; err != nil {
		return nil, err
	}
	aggs, err := monUITargetAggs(now)
	if err != nil {
		return nil, err
	}

	// Targets of an inbound that no longer exists are dropped: the cascade
	// (§3.6) normally removes them with the inbound, this only covers a row
	// that outlived it.
	byInbound := map[MonInboundRef][]MonUITarget{}
	for _, r := range rows {
		ref := MonInboundRef{r.InboundKind, r.InboundId}
		client, live := byId[r.MonClientId]
		t := MonUITarget{
			MonClientId: r.MonClientId, MonClientName: client.Name, Region: client.Region,
			ClientState: client.State,
			InboundKind: r.InboundKind, InboundId: r.InboundId, Path: r.Path,
			State: r.State, Since: r.Since, Reason: r.Reason,
			Retired: !live,
		}
		if t.MonClientName == "" {
			t.MonClientName = r.MonClientId
		}
		if a, ok := aggs[monUITargetKey{r.MonClientId, r.InboundKind, r.InboundId, r.Path}]; ok {
			if n := a.NOk + a.NFail; n > 0 {
				u := float64(a.NOk) / float64(n)
				t.Uptime24 = &u
			}
			bucketMs := a.BucketMs
			if bucketMs <= 0 {
				bucketMs = monBucketMs
			}
			expected := int(math.Ceil(float64(monUIWindow24.Milliseconds()) / float64(bucketMs)))
			t.Coverage24 = ratio(a.Received, expected)
			if a.LatWeight > 0 {
				lat := a.LatWeighted / a.LatWeight
				t.LatAvg24 = &lat
			}
		}
		byInbound[ref] = append(byInbound[ref], t)
	}

	out := &MonUITargets{
		Now: now.UnixMilli(), LastContact: s.MonLastContact(),
		Override: s.override(), MonClients: clients, Inbounds: []MonUIInbound{},
	}
	out.Stale, out.StaleSince = s.IsMonStale()
	for _, ib := range inbounds {
		targets := byInbound[MonInboundRef{ib.Kind, ib.InboundId}]
		if targets == nil {
			targets = []MonUITarget{}
		}
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].MonClientId != targets[j].MonClientId {
				return targets[i].MonClientId < targets[j].MonClientId
			}
			return targets[i].Path < targets[j].Path
		})
		out.Inbounds = append(out.Inbounds, MonUIInbound{
			MonInbound: ib, Worst: worstTargetState(targets), Targets: targets,
		})
	}
	return out, nil
}

// monUITargetAggs runs the one GROUP BY behind the 24h columns. lat_avg is
// NULL while a bucket saw no success, so the weighted average counts only the
// buckets that carry one.
func monUITargetAggs(now time.Time) (map[monUITargetKey]monUITargetAgg, error) {
	from := now.Add(-monUIWindow24).UnixMilli()
	to := now.UnixMilli()
	var rows []monUITargetAgg
	err := database.GetDB().Raw(`SELECT mon_client_id, inbound_kind, inbound_id, path,
			SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail, COUNT(*) AS received,
			MIN(bucket_ms) AS bucket_ms,
			SUM(CASE WHEN lat_avg IS NULL THEN 0 ELSE n_ok * lat_avg END) AS lat_weighted,
			SUM(CASE WHEN lat_avg IS NULL THEN 0 ELSE n_ok END) AS lat_weight
		FROM mon_stats_current
		WHERE bucket_start >= ? AND bucket_start < ?
		GROUP BY mon_client_id, inbound_kind, inbound_id, path`, from, to).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[monUITargetKey]monUITargetAgg, len(rows))
	for _, r := range rows {
		out[monUITargetKey{r.MonClientId, r.InboundKind, r.InboundId, r.Path}] = r
	}
	return out, nil
}

// worstTargetState folds the loaded targets of one inbound the way
// WorstLiveTargetState folds the stored ones — DOWN > FLAPPING > UNKNOWN > UP
// > PAUSED, retired mon-clients left out — without a query per inbound.
func worstTargetState(targets []MonUITarget) string {
	states := make([]string, 0, len(targets))
	for _, t := range targets {
		if !t.Retired {
			states = append(states, t.State)
		}
	}
	return worstOfStates(states)
}

// --- GET events --------------------------------------------------------------

// MonUIEvent is one feed entry: the stored event plus the names the page
// prints instead of ids.
type MonUIEvent struct {
	model.MonEvent
	MonClientName string `json:"monClientName"`
	Region        string `json:"region"`
	Retired       bool   `json:"retired"`
	InboundRemark string `json:"inboundRemark"`
}

// UIEvents is GET events (§7.4): the feed by ts desc, paged with before and
// limit, optionally narrowed to one inbound.
//
// before is exclusive and 0 means "from now"; limit is clamped to
// [1, 200] with 50 as the default. With an inbound filter only target events
// of that inbound come back — a mon_client or panel event carries no inbound
// and belongs to the unfiltered feed only.
func (s *MonitoringService) UIEvents(before int64, limit int, inboundKind string, inboundId int) ([]MonUIEvent, error) {
	if before <= 0 {
		before = time.Now().UnixMilli()
	}
	switch {
	case limit <= 0:
		limit = monUIEventsDefaultLimit
	case limit > monUIEventsMaxLimit:
		limit = monUIEventsMaxLimit
	}

	q := database.GetDB().Model(&model.MonEvent{}).Where("ts < ?", before)
	if inboundKind != "" {
		q = q.Where("kind = ? AND inbound_kind = ? AND inbound_id = ?", model.MonEventKindTarget, inboundKind, inboundId)
	}
	var rows []model.MonEvent
	// id breaks the tie: it is a uuid v7, so it orders by mint time within
	// the same millisecond.
	if err := q.Order("ts DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}

	clients := s.RegistrySnapshot()
	byId := make(map[string]MonClient, len(clients))
	for _, c := range clients {
		byId[c.Id] = c
	}
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	remarks := make(map[MonInboundRef]string, len(inbounds))
	for _, ib := range inbounds {
		remarks[MonInboundRef{ib.Kind, ib.InboundId}] = ib.Remark
	}

	out := make([]MonUIEvent, 0, len(rows))
	for _, r := range rows {
		e := MonUIEvent{MonEvent: r, InboundRemark: remarks[MonInboundRef{r.InboundKind, r.InboundId}]}
		if r.MonClientId != "" {
			c, live := byId[r.MonClientId]
			e.MonClientName, e.Region, e.Retired = c.Name, c.Region, !live
			if e.MonClientName == "" {
				e.MonClientName = r.MonClientId
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// --- GET stats ---------------------------------------------------------------

// MonUIPoint is one bucket of one series. A bucket that never arrived is a
// nil point, which the envelope renders as JSON null so the sparkline leaves
// a hole instead of drawing through it.
type MonUIPoint struct {
	T           int64  `json:"t"`
	NOk         int    `json:"nOk"`
	NFail       int    `json:"nFail"`
	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// MonUISeries is one (mon-client, path) line of the chart.
type MonUISeries struct {
	MonClientId   string        `json:"monClientId"`
	MonClientName string        `json:"monClientName"`
	Region        string        `json:"region"`
	Path          string        `json:"path"`
	Points        []*MonUIPoint `json:"points"`
}

// MonUIStats is GET stats: the window, the step every series is sampled at,
// which table it came from, and the series themselves.
type MonUIStats struct {
	From   int64         `json:"from"`
	To     int64         `json:"to"`
	StepMs int64         `json:"stepMs"`
	Source string        `json:"source"`
	Series []MonUISeries `json:"series"`
}

// monUIStatRow is one stored bucket, from either aggregate table.
type monUIStatRow struct {
	MonClientId string
	Path        string
	BucketStart int64
	NOk         int
	NFail       int
	LatMin      *int64
	LatAvg      *int64
	LatMax      *int64
	HandshakeMs *int64
}

// UIStats is GET stats (§7.4): the series of one inbound over the range the
// page asked for. Up to 24h the fine table answers at its own bucket width;
// beyond it the rollup table answers at the step configured right now, so
// rows left from a previous monRollupStepMinutes are ignored rather than
// drawn at the wrong width (§8).
//
// The window is snapped to the step: From = ⌊(now−rng)/step⌋·step and
// To = ⌊now/step⌋·step exclusive, so the partial bucket in progress is not
// drawn as a dip. Every series has exactly (To−From)/step points, missing
// buckets included as nil.
func (s *MonitoringService) UIStats(now time.Time, inboundKind string, inboundId int, rng time.Duration) (*MonUIStats, error) {
	if rng <= 0 {
		rng = monUIWindow24
	}
	source := MonUISourceRollup
	stepMs := s.rollupStepMs()
	if rng <= monSummaryWindowCurrent {
		source = MonUISourceCurrent
		var err error
		if stepMs, err = s.currentBucketMs(inboundKind, inboundId); err != nil {
			return nil, err
		}
	}
	if stepMs <= 0 {
		stepMs = monBucketMs
	}

	to := floorTo(now.UnixMilli(), stepMs)
	from := floorTo(now.Add(-rng).UnixMilli(), stepMs)
	out := &MonUIStats{From: from, To: to, StepMs: stepMs, Source: source, Series: []MonUISeries{}}
	n := int((to - from) / stepMs)
	if n <= 0 {
		return out, nil
	}

	rows, err := s.uiStatRows(source, inboundKind, inboundId, from, to, stepMs)
	if err != nil {
		return nil, err
	}

	clients := s.RegistrySnapshot()
	byId := make(map[string]MonClient, len(clients))
	for _, c := range clients {
		byId[c.Id] = c
	}
	type seriesKey struct{ MonClientId, Path string }
	index := map[seriesKey]int{}
	for _, r := range rows {
		k := seriesKey{r.MonClientId, r.Path}
		i, ok := index[k]
		if !ok {
			c := byId[r.MonClientId]
			name := c.Name
			if name == "" {
				name = r.MonClientId
			}
			i = len(out.Series)
			index[k] = i
			out.Series = append(out.Series, MonUISeries{
				MonClientId: r.MonClientId, MonClientName: name, Region: c.Region,
				Path: r.Path, Points: make([]*MonUIPoint, n),
			})
		}
		slot := int((r.BucketStart - from) / stepMs)
		if slot < 0 || slot >= n {
			continue
		}
		out.Series[i].Points[slot] = &MonUIPoint{
			T: from + int64(slot)*stepMs, NOk: r.NOk, NFail: r.NFail,
			LatMin: r.LatMin, LatAvg: r.LatAvg, LatMax: r.LatMax, HandshakeMs: r.HandshakeMs,
		}
	}
	sort.Slice(out.Series, func(i, j int) bool {
		if out.Series[i].MonClientId != out.Series[j].MonClientId {
			return out.Series[i].MonClientId < out.Series[j].MonClientId
		}
		return out.Series[i].Path < out.Series[j].Path
	})
	return out, nil
}

// uiStatRows reads the buckets of one inbound from whichever table the range
// chose.
func (s *MonitoringService) uiStatRows(source, inboundKind string, inboundId int, from, to, stepMs int64) ([]monUIStatRow, error) {
	var rows []monUIStatRow
	db := database.GetDB()
	if source == MonUISourceCurrent {
		err := db.Raw(`SELECT mon_client_id, path, bucket_start, n_ok, n_fail, lat_min, lat_avg, lat_max, handshake_ms
			FROM mon_stats_current
			WHERE inbound_kind = ? AND inbound_id = ? AND bucket_start >= ? AND bucket_start < ?`,
			inboundKind, inboundId, from, to).Scan(&rows).Error
		return rows, err
	}
	err := db.Raw(`SELECT mon_client_id, path, bucket_start, n_ok, n_fail, lat_min, lat_avg, lat_max, handshake_ms
		FROM mon_stats_rollup
		WHERE inbound_kind = ? AND inbound_id = ? AND step_ms = ? AND bucket_start >= ? AND bucket_start < ?`,
		inboundKind, inboundId, stepMs, from, to).Scan(&rows).Error
	return rows, err
}

// currentBucketMs is the finest bucket width stored for one inbound, which is
// the step its fine-grained series are drawn at. An inbound nobody has
// reported on yet gets the v1 default, so the page still draws an empty grid
// of the right shape.
func (s *MonitoringService) currentBucketMs(inboundKind string, inboundId int) (int64, error) {
	var min *int64
	err := database.GetDB().Raw(`SELECT MIN(bucket_ms) FROM mon_stats_current WHERE inbound_kind = ? AND inbound_id = ?`,
		inboundKind, inboundId).Scan(&min).Error
	if err != nil {
		return 0, err
	}
	if min == nil || *min <= 0 {
		return monBucketMs, nil
	}
	return *min, nil
}

// floorTo rounds a timestamp down to a multiple of step.
func floorTo(ms, step int64) int64 {
	if step <= 0 {
		return ms
	}
	rem := ms % step
	if rem < 0 {
		rem += step
	}
	return ms - rem
}
