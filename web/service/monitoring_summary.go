package service

import (
	"math"
	"sort"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// Uptime, coverage and incidents over a sliding window
// (monitoring-panel.md §6 and §7.4). One calculation serves two readers: the
// daily Telegram digest built by tgbot_monitoring.go, and the GET summary
// handler of the Monitoring page. Both must show the same numbers, so the
// arithmetic lives here and nowhere else.
//
// The rules the spec fixes:
//   - uptime = Σ n_ok / Σ (n_ok + n_fail) per (inbound, path); a bucket that
//     never arrived counts neither as success nor as failure, it only lowers
//     coverage.
//   - coverage = buckets received / buckets expected, where expected is one
//     fine bucket per bucket width in the window for every mon-client that is
//     ONLINE now and probes that (inbound, path) — it has a target there or
//     sent buckets for it — and received counts the buckets of those same
//     mon-clients. An OFFLINE mon-client owes nothing, and one that has never
//     sent a heartbeat (NEVER) is listed apart rather than as offline
//     (SBKubric/3ax-ui-monitoring#50, item 7).
//   - an incident is a target event with to_state = DOWN inside the window.

// monSummaryWindowCurrent is the longest window still served from
// mon_stats_current; beyond it the rollup table is the cheaper source.
const monSummaryWindowCurrent = 24 * time.Hour

// monWorstMinCoverage is how much of a target's expected buckets must have
// arrived before its uptime is trusted enough to be called the worst one.
const monWorstMinCoverage = 0.5

// MonSummaryPath is one path (direct, proxy or a hop of the chain) of one
// inbound, folded over every mon-client. Uptime is nil when no probe result landed in the window
// at all, which is not the same as 0 % uptime.
type MonSummaryPath struct {
	Path     string   `json:"path"`
	NOk      int      `json:"nOk"`
	NFail    int      `json:"nFail"`
	Uptime   *float64 `json:"uptime"`
	Received int      `json:"received"`
	Expected int      `json:"expected"`
	Coverage float64  `json:"coverage"`
}

// MonSummaryTarget is one (mon-client, inbound, path) probe, used for the
// "worst target" line of the digest and of the Monitoring page.
type MonSummaryTarget struct {
	MonClientId   string  `json:"monClientId"`
	MonClientName string  `json:"monClientName"`
	Region        string  `json:"region"`
	InboundKind   string  `json:"inboundKind"`
	InboundId     int     `json:"inboundId"`
	Path          string  `json:"path"`
	Uptime        float64 `json:"uptime"`
	Coverage      float64 `json:"coverage"`
}

// MonSummaryInbound is one enabled inbound. Paths holds direct, then proxy,
// then hop paths (edge:<name>, inner:<name>) by name, and only the ones with
// data; an inbound probed by nobody still appears, with no paths at all.
type MonSummaryInbound struct {
	Kind      string           `json:"kind"`
	InboundId int              `json:"inboundId"`
	Remark    string           `json:"remark"`
	Paths     []MonSummaryPath `json:"paths"`
	Coverage  float64          `json:"coverage"`
	Incidents int              `json:"incidents"`
}

// MonSummary is the whole window: per-inbound availability, the worst target,
// the mon-clients that are offline right now, those still waiting for their
// first heartbeat, and how long the panel considered monitoring silent.
type MonSummary struct {
	From            int64               `json:"from"`
	To              int64               `json:"to"`
	Inbounds        []MonSummaryInbound `json:"inbounds"`
	Worst           *MonSummaryTarget   `json:"worst"`
	OfflineClients  []MonClient         `json:"offlineClients"`
	AwaitingClients []MonClient         `json:"awaitingClients"`
	StaleMs         int64               `json:"staleMs"`
	StaleNow        bool                `json:"staleNow"`
}

// Registry states of a mon-client as mon-server reports them in the snapshot.
const (
	monClientOnline = "ONLINE"
	monClientNever  = "NEVER"
)

// monSummaryRow is one GROUP BY row: everything one mon-client measured for
// one (inbound, path) inside the window.
type monSummaryRow struct {
	InboundKind string
	InboundId   int
	Path        string
	MonClientId string
	NOk         int
	NFail       int
	Received    int
	StepMs      int64
}

// monSummaryPathRank puts direct and proxy first; every other path (the hops
// of the chain) follows by name.
var monSummaryPathRank = map[string]int{model.MonPathDirect: 0, model.MonPathProxy: 1}

// orderedSummaryPaths is the order paths appear in, everywhere.
func orderedSummaryPaths[V any](paths map[string]V) []string {
	out := make([]string, 0, len(paths))
	for p := range paths {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, iKnown := monSummaryPathRank[out[i]]
		rj, jKnown := monSummaryPathRank[out[j]]
		switch {
		case iKnown && jKnown:
			return ri < rj
		case iKnown != jKnown:
			return iKnown
		}
		return out[i] < out[j]
	})
	return out
}

// summaryProbing is, per (inbound, path), the ONLINE mon-clients that probe
// it: the ones with a target row there, plus any that sent buckets for it in
// the window (rows).
func summaryProbing(rows []monSummaryRow, online map[string]bool) (map[MonInboundRef]map[string]map[string]bool, error) {
	var targets []model.MonTarget
	if err := database.GetDB().Select("mon_client_id", "inbound_kind", "inbound_id", "path").Find(&targets).Error; err != nil {
		return nil, err
	}
	out := map[MonInboundRef]map[string]map[string]bool{}
	add := func(ref MonInboundRef, path, client string) {
		if !online[client] {
			return
		}
		if out[ref] == nil {
			out[ref] = map[string]map[string]bool{}
		}
		if out[ref][path] == nil {
			out[ref][path] = map[string]bool{}
		}
		out[ref][path][client] = true
	}
	for _, t := range targets {
		add(MonInboundRef{t.InboundKind, t.InboundId}, t.Path, t.MonClientId)
	}
	for _, r := range rows {
		add(MonInboundRef{r.InboundKind, r.InboundId}, r.Path, r.MonClientId)
	}
	return out, nil
}

// Summary folds the window [now−window, now) into the numbers the digest and
// the summary handler print. Windows up to 24h read mon_stats_current;
// longer ones read mon_stats_rollup at the step currently configured, so
// rows left over from a previous monRollupStepMinutes are ignored rather
// than double-counted.
func (s *MonitoringService) Summary(now time.Time, window time.Duration) (*MonSummary, error) {
	if window <= 0 {
		window = monSummaryWindowCurrent
	}
	to := now.UnixMilli()
	from := now.Add(-window).UnixMilli()

	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	clients := s.RegistrySnapshot()

	rows, fineStepMs, err := s.summaryRows(from, to, window)
	if err != nil {
		return nil, err
	}
	incidents, err := summaryIncidents(from, to)
	if err != nil {
		return nil, err
	}

	// Expected buckets per mon-client per (inbound, path): one fine bucket per
	// step of the window, whichever table the sums came from.
	perClient := int(math.Ceil(float64(window.Milliseconds()) / float64(fineStepMs)))
	if perClient < 1 {
		perClient = 1
	}

	names := map[string]MonClient{}
	online := map[string]bool{}
	offline := []MonClient{}
	awaiting := []MonClient{}
	for _, c := range clients {
		names[c.Id] = c
		switch c.State {
		case monClientOnline:
			online[c.Id] = true
		case monClientNever:
			awaiting = append(awaiting, c)
		default:
			offline = append(offline, c)
		}
	}
	probing, err := summaryProbing(rows, online)
	if err != nil {
		return nil, err
	}

	byPath := map[MonInboundRef]map[string]*MonSummaryPath{}
	byTarget := map[MonInboundRef]map[string][]monSummaryRow{}
	for _, r := range rows {
		ref := MonInboundRef{r.InboundKind, r.InboundId}
		if byPath[ref] == nil {
			byPath[ref] = map[string]*MonSummaryPath{}
			byTarget[ref] = map[string][]monSummaryRow{}
		}
		p := byPath[ref][r.Path]
		if p == nil {
			p = &MonSummaryPath{Path: r.Path, Expected: perClient * len(probing[ref][r.Path])}
			byPath[ref][r.Path] = p
		}
		p.NOk += r.NOk
		p.NFail += r.NFail
		if online[r.MonClientId] {
			p.Received += r.Received
		}
		byTarget[ref][r.Path] = append(byTarget[ref][r.Path], r)
	}

	out := &MonSummary{From: from, To: to, Inbounds: []MonSummaryInbound{}, OfflineClients: offline, AwaitingClients: awaiting}
	for _, ib := range inbounds {
		if !ib.Enable {
			continue
		}
		ref := MonInboundRef{ib.Kind, ib.InboundId}
		sum := MonSummaryInbound{Kind: ib.Kind, InboundId: ib.InboundId, Remark: ib.Remark,
			Paths: []MonSummaryPath{}, Incidents: incidents[ref]}
		var received, expected int
		paths := orderedSummaryPaths(byPath[ref])
		for _, path := range paths {
			p := byPath[ref][path]
			if n := p.NOk + p.NFail; n > 0 {
				u := float64(p.NOk) / float64(n)
				p.Uptime = &u
			}
			p.Coverage = ratio(p.Received, p.Expected)
			received += p.Received
			expected += p.Expected
			sum.Paths = append(sum.Paths, *p)
		}
		sum.Coverage = ratio(received, expected)
		out.Inbounds = append(out.Inbounds, sum)

		// The worst target is looked for among the same enabled inbounds, in
		// the same order, so a tie always resolves to the first of
		// (kind, inboundId, path, monClientId).
		for _, path := range paths {
			targets := byTarget[ref][path]
			sort.Slice(targets, func(i, j int) bool { return targets[i].MonClientId < targets[j].MonClientId })
			for _, r := range targets {
				n := r.NOk + r.NFail
				if n == 0 {
					continue
				}
				cov := ratio(r.Received, perClient)
				if cov < monWorstMinCoverage {
					continue
				}
				uptime := float64(r.NOk) / float64(n)
				if out.Worst != nil && uptime >= out.Worst.Uptime {
					continue
				}
				c := names[r.MonClientId]
				name, region := c.Name, c.Region
				if name == "" {
					name = r.MonClientId
				}
				out.Worst = &MonSummaryTarget{
					MonClientId: r.MonClientId, MonClientName: name, Region: region,
					InboundKind: ib.Kind, InboundId: ib.InboundId, Path: path,
					Uptime: uptime, Coverage: cov,
				}
			}
		}
	}

	out.StaleMs = s.StaleMsWithin(from, to, now)
	out.StaleNow, _ = s.IsMonStale()
	return out, nil
}

// summaryRows runs the one GROUP BY behind the whole summary and reports the
// fine bucket width coverage is measured in. Up to 24h that is the bucket_ms
// of the current rows themselves; beyond it the rollup rows carry n_buckets,
// which counts fine buckets too, so the width is taken from the current table
// (or the v1 default when it is empty).
func (s *MonitoringService) summaryRows(from, to int64, window time.Duration) ([]monSummaryRow, int64, error) {
	db := database.GetDB()
	var rows []monSummaryRow

	if window <= monSummaryWindowCurrent {
		err := db.Raw(`SELECT inbound_kind, inbound_id, path, mon_client_id,
				SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail,
				COUNT(*) AS received, MIN(bucket_ms) AS step_ms
			FROM mon_stats_current
			WHERE bucket_start >= ? AND bucket_start < ?
			GROUP BY inbound_kind, inbound_id, path, mon_client_id`, from, to).Scan(&rows).Error
		if err != nil {
			return nil, 0, err
		}
		step := int64(0)
		for _, r := range rows {
			if r.StepMs > 0 && (step == 0 || r.StepMs < step) {
				step = r.StepMs
			}
		}
		if step == 0 {
			step = monBucketMs
		}
		return rows, step, nil
	}

	stepMs := s.rollupStepMs()
	err := db.Raw(`SELECT inbound_kind, inbound_id, path, mon_client_id,
			SUM(n_ok) AS n_ok, SUM(n_fail) AS n_fail, SUM(n_buckets) AS received
		FROM mon_stats_rollup
		WHERE step_ms = ? AND bucket_start >= ? AND bucket_start < ?
		GROUP BY inbound_kind, inbound_id, path, mon_client_id`, stepMs, from, to).Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	fine := monBucketMs
	var min *int64
	if err := db.Raw(`SELECT MIN(bucket_ms) FROM mon_stats_current`).Scan(&min).Error; err != nil {
		return nil, 0, err
	}
	if min != nil && *min > 0 {
		fine = *min
	}
	return rows, fine, nil
}

// summaryIncidents counts the DOWN transitions of every inbound in the window.
func summaryIncidents(from, to int64) (map[MonInboundRef]int, error) {
	type row struct {
		InboundKind string
		InboundId   int
		Incidents   int
	}
	var rows []row
	err := database.GetDB().Raw(`SELECT inbound_kind, inbound_id, COUNT(*) AS incidents
		FROM mon_events
		WHERE kind = ? AND to_state = ? AND ts >= ? AND ts < ?
		GROUP BY inbound_kind, inbound_id`,
		model.MonEventKindTarget, model.MonStateDown, from, to).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[MonInboundRef]int, len(rows))
	for _, r := range rows {
		out[MonInboundRef{r.InboundKind, r.InboundId}] = r.Incidents
	}
	return out, nil
}

// ratio is received/expected clamped to [0,1]; an expectation of nothing is
// no coverage rather than full coverage.
func ratio(received, expected int) float64 {
	if expected <= 0 {
		return 0
	}
	v := float64(received) / float64(expected)
	if v > 1 {
		return 1
	}
	if v < 0 {
		return 0
	}
	return v
}
