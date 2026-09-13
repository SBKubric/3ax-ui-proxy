package service

import (
	"sort"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// Summary is the one calculation behind the daily Telegram digest and GET
// /panel/api/monitoring/summary (docs/spec/monitoring-panel.md §6, §7.4):
// per inbound and path, uptime = Σ n_ok / Σ (n_ok + n_fail) over every
// mon-client, coverage = buckets received / buckets expected, incidents =
// target events to DOWN; the worst target among those with at least half
// coverage; the mon-clients OFFLINE right now; the time the panel was STALE.
// Missing buckets are neither success nor failure.

// MonSummaryPath is one path of one inbound over the window.
type MonSummaryPath struct {
	Path      string   `json:"path"`
	NOk       int64    `json:"nOk"`
	NFail     int64    `json:"nFail"`
	Uptime    *float64 `json:"uptime"` // 0..1; null when no probe ran
	Coverage  float64  `json:"coverage"`
	Incidents int      `json:"incidents"`
}

// MonSummaryInbound is one inbound over the window.
type MonSummaryInbound struct {
	Kind      string           `json:"kind"`
	InboundId int              `json:"inboundId"`
	Remark    string           `json:"remark"`
	Paths     []MonSummaryPath `json:"paths"`
	Incidents int              `json:"incidents"`
}

// MonSummaryTarget is the worst target of the window.
type MonSummaryTarget struct {
	MonClientId   string  `json:"monClientId"`
	MonClientName string  `json:"monClientName"`
	Region        string  `json:"region"`
	Kind          string  `json:"kind"`
	InboundId     int     `json:"inboundId"`
	Remark        string  `json:"remark"`
	Path          string  `json:"path"`
	Uptime        float64 `json:"uptime"`
	Coverage      float64 `json:"coverage"`
}

// MonSummary is the window's summary.
type MonSummary struct {
	From              int64               `json:"from"`
	To                int64               `json:"to"`
	Inbounds          []MonSummaryInbound `json:"inbounds"`
	Worst             *MonSummaryTarget   `json:"worst"`
	OfflineMonClients []MonClient         `json:"offlineMonClients"`
	StaleMs           int64               `json:"staleMs"`
}

// monSeriesTotals is one series' sums over the window.
type monSeriesTotals struct {
	MonClientId string
	InboundKind string
	InboundId   int
	Path        string
	Buckets     int64
	NOk         int64
	NFail       int64
}

// seriesTotals sums the fine rows when the window fits the fine retention
// and the rollup rows otherwise, and returns the sums with the bucket width
// the rows have.
func (s *MonitoringService) seriesTotals(from, to int64, now time.Time) ([]monSeriesTotals, int64, error) {
	floor, err := s.retentionFloor(now)
	if err != nil {
		return nil, 0, err
	}
	db := database.GetDB()
	var totals []monSeriesTotals
	if from >= floor {
		err = db.Raw(`SELECT mon_client_id, inbound_kind, inbound_id, path, COUNT(*) AS buckets, SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail
			FROM mon_stats_current WHERE bucket_start >= ? AND bucket_start < ?
			GROUP BY mon_client_id, inbound_kind, inbound_id, path`, from, to).Scan(&totals).Error
		return totals, model.MonBucketMs, err
	}
	stepMs, err := s.rollupStepMs()
	if err != nil {
		return nil, 0, err
	}
	err = db.Raw(`SELECT mon_client_id, inbound_kind, inbound_id, path, COUNT(*) AS buckets, SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail
		FROM mon_stats_rollup WHERE step_ms = ? AND bucket_start >= ? AND bucket_start < ?
		GROUP BY mon_client_id, inbound_kind, inbound_id, path`, stepMs, from, to).Scan(&totals).Error
	return totals, stepMs, err
}

// Summary computes the window [now − window, now).
func (s *MonitoringService) Summary(now time.Time, window time.Duration) (*MonSummary, error) {
	to := now.UnixMilli()
	from := now.Add(-window).UnixMilli()
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	totals, bucketMs, err := s.seriesTotals(from, to, now)
	if err != nil {
		return nil, err
	}
	expectedPerSeries := float64(window.Milliseconds()) / float64(bucketMs)

	type incidentRow struct {
		InboundKind string
		InboundId   int
		Path        string
		N           int
	}
	var incidentRows []incidentRow
	if err := database.GetDB().Raw(`SELECT inbound_kind, inbound_id, path, COUNT(*) AS n FROM mon_events
		WHERE kind = ? AND to_state = ? AND ts >= ? AND ts < ? GROUP BY inbound_kind, inbound_id, path`,
		model.MonEventTarget, model.MonStateDown, from, to).Scan(&incidentRows).Error; err != nil {
		return nil, err
	}
	incidents := map[MonInboundRef]map[string]int{}
	for _, r := range incidentRows {
		ref := MonInboundRef{Kind: r.InboundKind, InboundId: r.InboundId}
		if incidents[ref] == nil {
			incidents[ref] = map[string]int{}
		}
		incidents[ref][r.Path] = r.N
	}

	snapshot := s.Snapshot()
	monClients := make(map[string]MonClient, len(snapshot))
	for _, c := range snapshot {
		monClients[c.Id] = c
	}

	type pathAgg struct {
		nOk, nFail, buckets int64
		series              int
	}
	agg := map[MonInboundRef]map[string]*pathAgg{}
	var worst *MonSummaryTarget
	remarks := map[MonInboundRef]string{}
	for _, ib := range inbounds {
		remarks[MonInboundRef{Kind: ib.Kind, InboundId: ib.InboundId}] = ib.Remark
	}
	for _, t := range totals {
		ref := MonInboundRef{Kind: t.InboundKind, InboundId: t.InboundId}
		if _, known := remarks[ref]; !known {
			continue
		}
		if agg[ref] == nil {
			agg[ref] = map[string]*pathAgg{}
		}
		if agg[ref][t.Path] == nil {
			agg[ref][t.Path] = &pathAgg{}
		}
		a := agg[ref][t.Path]
		a.nOk += t.NOk
		a.nFail += t.NFail
		a.buckets += t.Buckets
		a.series++

		probes := t.NOk + t.NFail
		coverage := float64(t.Buckets) / expectedPerSeries
		mc, live := monClients[t.MonClientId]
		if !live || probes == 0 || coverage < 0.5 {
			continue
		}
		uptime := float64(t.NOk) / float64(probes)
		if worst == nil || uptime < worst.Uptime {
			worst = &MonSummaryTarget{MonClientId: t.MonClientId, MonClientName: mc.Name, Region: mc.Region,
				Kind: t.InboundKind, InboundId: t.InboundId, Remark: remarks[ref], Path: t.Path, Uptime: uptime, Coverage: coverage}
		}
	}

	out := &MonSummary{From: from, To: to, Inbounds: make([]MonSummaryInbound, 0, len(inbounds)), Worst: worst,
		OfflineMonClients: []MonClient{}, StaleMs: s.staleWithin(from, to)}
	for _, ib := range inbounds {
		ref := MonInboundRef{Kind: ib.Kind, InboundId: ib.InboundId}
		entry := MonSummaryInbound{Kind: ib.Kind, InboundId: ib.InboundId, Remark: ib.Remark, Paths: []MonSummaryPath{}}
		for _, path := range []string{model.MonPathDirect, model.MonPathProxy} {
			a := agg[ref][path]
			n := incidents[ref][path]
			entry.Incidents += n
			if a == nil {
				if n > 0 {
					entry.Paths = append(entry.Paths, MonSummaryPath{Path: path, Incidents: n})
				}
				continue
			}
			p := MonSummaryPath{Path: path, NOk: a.nOk, NFail: a.nFail, Incidents: n,
				Coverage: float64(a.buckets) / (expectedPerSeries * float64(a.series))}
			if probes := a.nOk + a.nFail; probes > 0 {
				u := float64(a.nOk) / float64(probes)
				p.Uptime = &u
			}
			entry.Paths = append(entry.Paths, p)
		}
		out.Inbounds = append(out.Inbounds, entry)
	}
	for _, c := range snapshot {
		if c.State == "OFFLINE" {
			out.OfflineMonClients = append(out.OfflineMonClients, c)
		}
	}
	sort.Slice(out.OfflineMonClients, func(i, j int) bool { return out.OfflineMonClients[i].Id < out.OfflineMonClients[j].Id })
	return out, nil
}

// monStaleSpan is one period the panel spent STALE.
type monStaleSpan struct {
	since, until int64
}

// staleWithin sums the STALE time inside [from, to): closed spans remembered
// in memory since the panel started, plus the open one. The panel stores no
// STALE history: this is the only state it derives, and a restart forgets it.
func (s *MonitoringService) staleWithin(from, to int64) int64 {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	var total int64
	spans := monRuntime.staleSpans
	if monRuntime.stale {
		spans = append(spans, monStaleSpan{since: monRuntime.staleSince, until: to})
	}
	for _, span := range spans {
		start, end := max(span.since, from), min(span.until, to)
		if end > start {
			total += end - start
		}
	}
	return total
}

// rememberStaleSpan records a closed STALE period and forgets those older
// than the longest window the summary serves.
func rememberStaleSpan(since, until int64) {
	const keep = 7 * 24 * 3_600_000
	monRuntime.staleSpans = append(monRuntime.staleSpans, monStaleSpan{since: since, until: until})
	kept := monRuntime.staleSpans[:0]
	for _, span := range monRuntime.staleSpans {
		if span.until >= until-keep {
			kept = append(kept, span)
		}
	}
	monRuntime.staleSpans = kept
}
