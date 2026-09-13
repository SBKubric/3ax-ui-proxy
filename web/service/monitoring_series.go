package service

import (
	"fmt"
	"sort"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// Time series for the Monitoring page (docs/spec/monitoring-panel.md §7.4,
// GET stats): one series per mon-client × path, one point per bucket of the
// range, null where no bucket arrived. Ranges up to a day come from the
// 5-minute rows, longer ones from the rollup, and the response says which
// step it used so the UI never guesses.

// MonPoint is one bucket of a series.
type MonPoint struct {
	T           int64  `json:"t"`
	NOk         int    `json:"nOk"`
	NFail       int    `json:"nFail"`
	LatMin      *int64 `json:"latMin"`
	LatAvg      *int64 `json:"latAvg"`
	LatMax      *int64 `json:"latMax"`
	HandshakeMs *int64 `json:"handshakeMs"`
}

// MonSeries is one mon-client probing the inbound over one path.
type MonSeries struct {
	MonClientId string      `json:"monClientId"`
	Path        string      `json:"path"`
	Points      []*MonPoint `json:"points"`
}

// MonSeriesResponse is the body of GET stats.
type MonSeriesResponse struct {
	Kind      string      `json:"kind"`
	InboundId int         `json:"inboundId"`
	Range     string      `json:"range"`
	From      int64       `json:"from"`
	To        int64       `json:"to"`
	StepMs    int64       `json:"stepMs"`
	Series    []MonSeries `json:"series"`
}

// monRanges maps the range keys the UI offers to their windows.
var monRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// MonRangeWindow resolves a range key; the second value is false for an
// unknown key.
func MonRangeWindow(key string) (time.Duration, bool) {
	w, ok := monRanges[key]
	return w, ok
}

// Series builds the series of one inbound over a range ending now.
func (s *MonitoringService) Series(kind string, inboundId int, rangeKey string, now time.Time) (*MonSeriesResponse, error) {
	window, ok := monRanges[rangeKey]
	if !ok {
		return nil, fmt.Errorf("unknown range %q", rangeKey)
	}
	if !validKind(kind) {
		return nil, fmt.Errorf("unknown inbound kind %q", kind)
	}
	stepMs := model.MonBucketMs
	table := "mon_stats_current"
	var stepFilter []any
	if window > 24*time.Hour {
		var err error
		if stepMs, err = s.rollupStepMs(); err != nil {
			return nil, err
		}
		table = "mon_stats_rollup"
		stepFilter = []any{stepMs}
	}
	// The window ends at the close of the bucket now falls in, so the open
	// bucket (the hour in progress, for the rollup) is on the chart.
	to := now.UnixMilli()
	if rem := to % stepMs; rem != 0 {
		to += stepMs - rem
	}
	from := to - window.Milliseconds()

	type row struct {
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
	var rows []row
	query := "SELECT mon_client_id, path, bucket_start, n_ok, n_fail, lat_min, lat_avg, lat_max, handshake_ms FROM " + table +
		" WHERE inbound_kind = ? AND inbound_id = ? AND bucket_start >= ? AND bucket_start < ?"
	args := []any{kind, inboundId, from, to}
	if stepFilter != nil {
		query += " AND step_ms = ?"
		args = append(args, stepFilter...)
	}
	if err := database.GetDB().Raw(query+" ORDER BY mon_client_id, path, bucket_start", args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	nPoints := int(window.Milliseconds() / stepMs)
	series := map[[2]string]*MonSeries{}
	for _, r := range rows {
		key := [2]string{r.MonClientId, r.Path}
		sr := series[key]
		if sr == nil {
			sr = &MonSeries{MonClientId: r.MonClientId, Path: r.Path, Points: make([]*MonPoint, nPoints)}
			series[key] = sr
		}
		idx := int((r.BucketStart - from) / stepMs)
		if idx < 0 || idx >= nPoints {
			continue
		}
		sr.Points[idx] = &MonPoint{T: r.BucketStart, NOk: r.NOk, NFail: r.NFail,
			LatMin: r.LatMin, LatAvg: r.LatAvg, LatMax: r.LatMax, HandshakeMs: r.HandshakeMs}
	}
	out := &MonSeriesResponse{Kind: kind, InboundId: inboundId, Range: rangeKey, From: from, To: to, StepMs: stepMs, Series: []MonSeries{}}
	for _, sr := range series {
		out.Series = append(out.Series, *sr)
	}
	sort.Slice(out.Series, func(i, j int) bool {
		if out.Series[i].MonClientId != out.Series[j].MonClientId {
			return out.Series[i].MonClientId < out.Series[j].MonClientId
		}
		return out.Series[i].Path < out.Series[j].Path
	})
	return out, nil
}

// MonTargetView is a target row with what the page shows next to it.
type MonTargetView struct {
	model.MonTarget
	LatAvg24h *int64   `json:"latAvg24h"`
	Uptime24h *float64 `json:"uptime24h"`
	Cov24h    float64  `json:"cov24h"`
}

// MonInboundView is an inbound with its folded Health state.
type MonInboundView struct {
	MonInbound
	Health string `json:"health"`
}

// MonTargetsResponse is the body of GET targets: the page's whole picture in
// one request.
type MonTargetsResponse struct {
	Enabled     bool             `json:"enabled"`
	Stale       bool             `json:"stale"`
	StaleSince  int64            `json:"staleSince"`
	LastContact int64            `json:"lastContact"`
	Threshold   int              `json:"thresholdMinutes"`
	ServerTime  int64            `json:"serverTime"`
	Override    MonOverride      `json:"override"`
	Probe       MonProbeStatus   `json:"probe"`
	MonClients  []MonClient      `json:"monClients"`
	Inbounds    []MonInboundView `json:"inbounds"`
	Targets     []MonTargetView  `json:"targets"`
}

// MonProbeStatus describes the probe set for the settings page.
type MonProbeStatus struct {
	SubId       string `json:"subId"`
	LastEnsured int64  `json:"lastEnsured"`
	Count       int    `json:"count"`
}

// ProbeStatus reports the probe set for the settings page.
func (s *MonitoringService) ProbeStatus() (MonProbeStatus, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return MonProbeStatus{}, err
	}
	lastEnsured, err := s.settingService.GetMonProbeLastEnsured()
	if err != nil {
		return MonProbeStatus{}, err
	}
	count, err := s.ProbeAccountCount()
	if err != nil {
		return MonProbeStatus{}, err
	}
	return MonProbeStatus{SubId: subId, LastEnsured: lastEnsured, Count: count}, nil
}

// Targets assembles GET targets: the live target rows (mon-clients of the
// snapshot only) with their last day's uptime and latency, the snapshot,
// the STALE flag, and every inbound with its folded Health.
func (s *MonitoringService) Targets(now time.Time) (*MonTargetsResponse, error) {
	enabled, err := s.settingService.GetMonEnable()
	if err != nil {
		return nil, err
	}
	threshold, err := s.settingService.GetMonStaleMinutes()
	if err != nil {
		return nil, err
	}
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	states, err := s.WorstLiveTargetStates()
	if err != nil {
		return nil, err
	}
	probe, err := s.ProbeStatus()
	if err != nil {
		return nil, err
	}
	live := s.SnapshotIds()
	var targets []model.MonTarget
	if err := database.GetDB().Order("inbound_kind, inbound_id, mon_client_id, path").Find(&targets).Error; err != nil {
		return nil, err
	}

	// Last day per series, for the uptime and latency columns.
	type dayRow struct {
		MonClientId string
		InboundKind string
		InboundId   int
		Path        string
		Buckets     int64
		NOk         int64
		NFail       int64
		LatSum      *float64
		LatWeight   *int64
	}
	var days []dayRow
	dayFrom := now.Add(-24 * time.Hour).UnixMilli()
	if err := database.GetDB().Raw(`SELECT mon_client_id, inbound_kind, inbound_id, path, COUNT(*) AS buckets, SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail,
		SUM(CASE WHEN lat_avg IS NOT NULL THEN lat_avg * n_ok END) AS lat_sum, SUM(CASE WHEN lat_avg IS NOT NULL THEN n_ok END) AS lat_weight
		FROM mon_stats_current WHERE bucket_start >= ? GROUP BY mon_client_id, inbound_kind, inbound_id, path`, dayFrom).Scan(&days).Error; err != nil {
		return nil, err
	}
	dayBy := map[monSeriesKey]dayRow{}
	for _, d := range days {
		dayBy[monSeriesKey{d.MonClientId, d.InboundKind, d.InboundId, d.Path}] = d
	}
	expected := float64(24 * time.Hour / time.Millisecond / time.Duration(model.MonBucketMs))

	stale, staleSince := s.Stale()
	out := &MonTargetsResponse{
		Enabled: enabled, Stale: stale, StaleSince: staleSince, LastContact: s.LastContact(), Threshold: threshold,
		ServerTime: now.UnixMilli(), Override: s.override(), Probe: probe, MonClients: s.Snapshot(),
		Inbounds: make([]MonInboundView, 0, len(inbounds)), Targets: []MonTargetView{},
	}
	for _, ib := range inbounds {
		out.Inbounds = append(out.Inbounds, MonInboundView{MonInbound: ib, Health: states[MonInboundRef{Kind: ib.Kind, InboundId: ib.InboundId}]})
	}
	for _, t := range targets {
		if _, ok := live[t.MonClientId]; !ok {
			continue
		}
		view := MonTargetView{MonTarget: t}
		if d, ok := dayBy[monSeriesKey{t.MonClientId, t.InboundKind, t.InboundId, t.Path}]; ok {
			view.Cov24h = float64(d.Buckets) / expected
			if probes := d.NOk + d.NFail; probes > 0 {
				u := float64(d.NOk) / float64(probes)
				view.Uptime24h = &u
			}
			if d.LatSum != nil && d.LatWeight != nil && *d.LatWeight > 0 {
				v := int64(*d.LatSum / float64(*d.LatWeight))
				view.LatAvg24h = &v
			}
		}
		out.Targets = append(out.Targets, view)
	}
	return out, nil
}

// MonEventView is a feed entry with the inbound's remark and whether its
// mon-client has left the registry.
type MonEventView struct {
	model.MonEvent
	InboundRemark string `json:"inboundRemark"`
	Retired       bool   `json:"retired"`
}

// MonEventsMax caps GET events.
const MonEventsMax = 200

// Events is the feed page before a timestamp, newest first, optionally for
// one inbound; limit is clamped to MonEventsMax.
func (s *MonitoringService) Events(before int64, limit int, kind string, inboundId int) ([]MonEventView, error) {
	if limit <= 0 || limit > MonEventsMax {
		limit = MonEventsMax
	}
	q := database.GetDB().Model(&model.MonEvent{})
	if before > 0 {
		q = q.Where("ts < ?", before)
	}
	if kind != "" {
		q = q.Where("inbound_kind = ? AND inbound_id = ?", kind, inboundId)
	}
	var events []model.MonEvent
	if err := q.Order("ts desc").Limit(limit).Find(&events).Error; err != nil {
		return nil, err
	}
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	remarks := map[MonInboundRef]string{}
	for _, ib := range inbounds {
		remarks[MonInboundRef{Kind: ib.Kind, InboundId: ib.InboundId}] = ib.Remark
	}
	live := s.SnapshotIds()
	out := make([]MonEventView, 0, len(events))
	for _, e := range events {
		view := MonEventView{MonEvent: e}
		if e.Kind == model.MonEventTarget {
			view.InboundRemark = remarks[MonInboundRef{Kind: e.InboundKind, InboundId: e.InboundId}]
		}
		if e.MonClientId != "" {
			_, isLive := live[e.MonClientId]
			view.Retired = !isLive
		}
		out = append(out, view)
	}
	return out, nil
}
