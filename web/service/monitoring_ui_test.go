package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// monTarget stores one mon_targets row the way ApplyEvents would leave it.
func monTarget(t *testing.T, client, kind string, id int, path, state string, since int64, reason string) {
	t.Helper()
	row := model.MonTarget{MonClientId: client, InboundKind: kind, InboundId: id, Path: path,
		State: state, Since: since, Reason: reason}
	if err := database.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("create target %s/%s/%d/%s: %v", client, kind, id, path, err)
	}
}

// monCurrentLat is monCurrent with a latency: n buckets from start, each with
// the same lat_min/avg/max, or all three NULL when lat is negative.
func monCurrentLat(t *testing.T, client, kind string, id int, path string, start int64, n, ok, fail int, lat int64) {
	t.Helper()
	db := database.GetDB()
	for i := 0; i < n; i++ {
		row := model.MonStatsCurrent{
			MonClientId: client, InboundKind: kind, InboundId: id, Path: path,
			BucketStart: start + int64(i)*monBucketMs, BucketMs: monBucketMs, NOk: ok, NFail: fail,
		}
		if lat >= 0 {
			v := lat
			row.LatMin, row.LatAvg, row.LatMax = &v, &v, &v
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("create current bucket: %v", err)
		}
	}
}

// TestUITargetsEnrichesFoldsAndDrops walks the whole GET targets answer
// (§7.1): every inbound is a card even when it is disabled, targets carry the
// mon-client they came from, the 24h columns come from mon_stats_current, a
// mon-client that left the registry is retired and does not decide the Health
// badge, and a target of an inbound that no longer exists is dropped.
func TestUITargetsEnrichesFoldsAndDrops(t *testing.T) {
	m := newMonitoringTestService(t)
	t.Cleanup(resetMonStaleForTest)
	monInbound(t, 1, model.VLESS, true)
	monInbound(t, 2, model.VLESS, false)
	monRemark(t, 1, "Reality main")
	monRemark(t, 2, "Reality spare")
	monRegister(t,
		MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"},
		MonClient{Id: "fra-1", Name: "Frankfurt", Region: "eu-central", State: "OFFLINE"},
	)

	now := monSummaryNow
	from := now.Add(-24 * time.Hour).UnixMilli()
	monTarget(t, "ams-1", "xray", 1, "direct", model.MonStateUp, from+1000, "")
	monTarget(t, "fra-1", "xray", 1, "proxy", model.MonStateUp, from+2000, "")
	// A mon-client outside the snapshot: its DOWN must not reach the badge.
	monTarget(t, "ber-9", "xray", 1, "direct", model.MonStateDown, from+3000, "tls_timeout")
	// A target of an inbound that is gone: dropped, not attached anywhere.
	monTarget(t, "ams-1", "xray", 7, "direct", model.MonStateDown, from+4000, "refused")

	// 24h window of ams-1 on inbound 1: 100 buckets at 100 ms with one
	// failure each, 100 at 200 ms without, and one bucket that only failed
	// and therefore carries no latency at all.
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from, 100, 1, 1, 100)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from+100*monBucketMs, 100, 3, 0, 200)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from+200*monBucketMs, 1, 0, 5, -1)
	// Older than the window: invisible to every column.
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from-monBucketMs, 1, 0, 1000, 9999)

	out, err := m.UITargets(now)
	if err != nil {
		t.Fatalf("UITargets: %v", err)
	}
	if out.Now != now.UnixMilli() || out.Stale || out.StaleSince != 0 {
		t.Errorf("header = now %d stale %v/%d, want %d false/0", out.Now, out.Stale, out.StaleSince, now.UnixMilli())
	}
	if out.Override.Enabled || len(out.MonClients) != 2 {
		t.Errorf("override = %+v, monClients = %+v, want disabled and both clients", out.Override, out.MonClients)
	}
	if len(out.Inbounds) != 2 {
		t.Fatalf("inbounds = %d, want 2 (the disabled one is a card too)", len(out.Inbounds))
	}

	one := out.Inbounds[0]
	if one.InboundId != 1 || one.Remark != "Reality main" || !one.Enable {
		t.Errorf("first card = %+v, want enabled inbound 1 Reality main", one.MonInbound)
	}
	if one.Worst != model.MonStateUp {
		t.Errorf("worst = %q, want UP: the DOWN target belongs to a retired mon-client", one.Worst)
	}
	if len(one.Targets) != 3 {
		t.Fatalf("targets = %+v, want 3 (the one of the vanished inbound is dropped)", one.Targets)
	}
	for i, want := range []string{"ams-1", "ber-9", "fra-1"} {
		if one.Targets[i].MonClientId != want {
			t.Errorf("target %d = %s, want %s: sorted by (monClientId, path)", i, one.Targets[i].MonClientId, want)
		}
	}

	ams := one.Targets[0]
	if ams.MonClientName != "Amsterdam" || ams.Region != "eu-west" || ams.ClientState != "ONLINE" || ams.Retired {
		t.Errorf("ams-1 target = %+v, want the registry name, region, state and retired=false", ams)
	}
	if ams.State != model.MonStateUp || ams.Since != from+1000 {
		t.Errorf("ams-1 state = %s since %d, want UP since %d", ams.State, ams.Since, from+1000)
	}
	// 400 successes against 105 failures, 201 of the 288 buckets a
	// mon-client owes over 24h, latency weighted by n_ok: (100·1·100 +
	// 100·3·200) / 400 = 175 ms.
	if ams.Uptime24 == nil || !nearly(*ams.Uptime24, 400.0/505.0) {
		t.Errorf("uptime24 = %v, want %v", ams.Uptime24, 400.0/505.0)
	}
	if !nearly(ams.Coverage24, 201.0/288.0) {
		t.Errorf("coverage24 = %v, want %v", ams.Coverage24, 201.0/288.0)
	}
	if ams.LatAvg24 == nil || *ams.LatAvg24 != 175 {
		t.Errorf("latAvg24 = %v, want 175", ams.LatAvg24)
	}

	ber := one.Targets[1]
	if !ber.Retired || ber.MonClientName != "ber-9" || ber.ClientState != "" {
		t.Errorf("ber-9 target = %+v, want retired with the id as its name", ber)
	}
	if ber.Uptime24 != nil || ber.LatAvg24 != nil || ber.Coverage24 != 0 {
		t.Errorf("ber-9 24h columns = %v/%v/%v, want nil/nil/0 — nothing was measured", ber.Uptime24, ber.LatAvg24, ber.Coverage24)
	}

	two := out.Inbounds[1]
	if two.InboundId != 2 || two.Enable || two.Worst != "" || two.Targets == nil || len(two.Targets) != 0 {
		t.Errorf("second card = %+v worst %q targets %+v, want the disabled inbound with no targets", two.MonInbound, two.Worst, two.Targets)
	}
}

// monUIEventId mints a stable 36-character id per index, so ordering by id is
// ordering by index.
func monUIEventId(i int) string { return fmt.Sprintf("%036d", i) }

// TestUIEventsOrderPagingAndFilter: the feed is ts desc, before is exclusive,
// limit is clamped to 200, and an inbound filter keeps only target events of
// that inbound — the mon_client and panel events belong to the unfiltered
// feed alone.
func TestUIEventsOrderPagingAndFilter(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true)
	monRemark(t, 1, "Reality main")
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"})

	// The feed has no clock parameter — before = 0 means "now" — so the
	// fixtures hang off the wall clock, an hour back, and stay in the past
	// whenever the test runs.
	base := time.Now().Add(-time.Hour).UnixMilli()
	const n = 205
	for i := 0; i < n; i++ {
		monStoreEvent(t, monUIEventId(i), base+int64(i)*1000, model.MonEventKindTarget, "ams-1", "xray", 1, "direct", "UP", "DOWN")
	}
	// Two feed entries with no inbound, the newest of all.
	monStoreEvent(t, monUIEventId(1000), base+500000, model.MonEventKindMonClient, "old-1", "", 0, "", "ONLINE", "OFFLINE")
	monStoreEvent(t, monUIEventId(1001), base+600000, model.MonEventKindPanel, "", "", 0, "", "PANEL_UP", "PANEL_DOWN")

	all, err := m.UIEvents(0, 500, "", 0)
	if err != nil {
		t.Fatalf("UIEvents: %v", err)
	}
	if len(all) != 200 {
		t.Fatalf("limit 500 returned %d events, want the 200 the spec caps at", len(all))
	}
	if all[0].Kind != model.MonEventKindPanel || all[1].Kind != model.MonEventKindMonClient {
		t.Errorf("feed starts with %s, %s, want the panel and mon_client events — they are the newest", all[0].Kind, all[1].Kind)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Ts < all[i].Ts {
			t.Fatalf("event %d is older than %d: the feed is not ts desc", i-1, i)
		}
	}
	// The mon_client of a registry that no longer lists it is marked retired;
	// a panel event names no mon-client at all.
	if !all[1].Retired || all[1].MonClientName != "old-1" {
		t.Errorf("mon_client event = %+v, want retired with the id as its name", all[1])
	}
	if all[0].Retired || all[0].MonClientName != "" {
		t.Errorf("panel event = %+v, want no mon-client at all", all[0])
	}
	if all[2].MonClientName != "Amsterdam" || all[2].Region != "eu-west" || all[2].InboundRemark != "Reality main" {
		t.Errorf("target event = %+v, want the registry name/region and the inbound remark", all[2])
	}

	filtered, err := m.UIEvents(0, 500, "xray", 1)
	if err != nil {
		t.Fatalf("UIEvents filtered: %v", err)
	}
	if len(filtered) != 200 {
		t.Fatalf("filtered feed = %d events, want 200", len(filtered))
	}
	for _, e := range filtered {
		if e.Kind != model.MonEventKindTarget || e.InboundId != 1 {
			t.Fatalf("filtered feed carries %+v, want target events of inbound 1 only", e)
		}
	}
	if filtered[0].Ts != base+int64(n-1)*1000 {
		t.Errorf("filtered feed starts at %d, want the newest target event %d", filtered[0].Ts, base+int64(n-1)*1000)
	}

	// before is exclusive, and the default limit is 50.
	page, err := m.UIEvents(base+100*1000, 0, "xray", 1)
	if err != nil {
		t.Fatalf("UIEvents before: %v", err)
	}
	if len(page) != 50 {
		t.Fatalf("second page = %d events, want the default 50", len(page))
	}
	if page[0].Ts != base+99*1000 {
		t.Errorf("second page starts at %d, want %d — before is exclusive", page[0].Ts, base+99*1000)
	}

	empty, err := m.UIEvents(0, 10, "xray", 2)
	if err != nil {
		t.Fatalf("UIEvents empty: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("feed of an inbound with no events = %#v, want an empty slice, never null", empty)
	}
}

// TestUIStatsCurrentLeavesNullHoles: up to 24h the series come from
// mon_stats_current at its own bucket width, every bucket of the window has a
// slot, and the ones that never arrived stay nil so the sparkline draws a gap.
func TestUIStatsCurrentLeavesNullHoles(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true)
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"})

	now := monSummaryNow
	from := now.Add(-time.Hour).UnixMilli()
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from, 1, 5, 0, 41)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from+5*monBucketMs, 1, 4, 1, 55)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from+11*monBucketMs, 1, 5, 0, 44)
	monCurrentLat(t, "ams-1", "xray", 1, "proxy", from+3*monBucketMs, 1, 5, 0, 60)
	// A mon-client outside the registry still gets its own series.
	monCurrentLat(t, "old-1", "xray", 1, "direct", from+2*monBucketMs, 1, 1, 0, 70)
	// Out of the window on both ends, and a bucket of another inbound.
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from-monBucketMs, 1, 9, 9, 999)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", now.UnixMilli(), 1, 9, 9, 999)
	monCurrentLat(t, "ams-1", "xray", 2, "direct", from, 1, 9, 9, 999)
	// The rollup table must not be consulted for a one-hour range.
	monRollup(t, "ams-1", "xray", 1, "direct", from, 3600000, 1, 12, 100, 0)

	out, err := m.UIStats(now, "xray", 1, time.Hour)
	if err != nil {
		t.Fatalf("UIStats: %v", err)
	}
	if out.Source != MonUISourceCurrent || out.StepMs != monBucketMs {
		t.Errorf("source = %s step = %d, want current at %d", out.Source, out.StepMs, monBucketMs)
	}
	if out.From != from || out.To != now.UnixMilli() {
		t.Errorf("window = [%d,%d), want [%d,%d)", out.From, out.To, from, now.UnixMilli())
	}
	if len(out.Series) != 3 {
		t.Fatalf("series = %d, want 3: ams-1 on both paths and the retired old-1", len(out.Series))
	}
	for i, want := range [][2]string{{"ams-1", "direct"}, {"ams-1", "proxy"}, {"old-1", "direct"}} {
		if out.Series[i].MonClientId != want[0] || out.Series[i].Path != want[1] {
			t.Errorf("series %d = %s/%s, want %s/%s: sorted by (monClientId, path)",
				i, out.Series[i].MonClientId, out.Series[i].Path, want[0], want[1])
		}
	}
	if out.Series[2].MonClientName != "old-1" {
		t.Errorf("retired series name = %q, want the id as a fallback", out.Series[2].MonClientName)
	}

	direct := out.Series[0]
	if len(direct.Points) != 12 {
		t.Fatalf("points = %d, want 12 buckets of five minutes in an hour", len(direct.Points))
	}
	for i, p := range direct.Points {
		filled := i == 0 || i == 5 || i == 11
		if filled != (p != nil) {
			t.Errorf("point %d = %v, want filled=%v", i, p, filled)
		}
	}
	if p := direct.Points[5]; p.T != from+5*monBucketMs || p.NOk != 4 || p.NFail != 1 || p.LatAvg == nil || *p.LatAvg != 55 {
		t.Errorf("point 5 = %+v, want the bucket at %d with 4/1 and 55 ms", p, from+5*monBucketMs)
	}
	if p := out.Series[1].Points[3]; p == nil || p.NOk != 5 {
		t.Errorf("proxy point 3 = %v, want the stored bucket", p)
	}

	// An inbound nobody reported on is an empty grid, not a null series list.
	none, err := m.UIStats(now, "awg", 0, time.Hour)
	if err != nil {
		t.Fatalf("UIStats of an unprobed inbound: %v", err)
	}
	if none.Series == nil || len(none.Series) != 0 || none.StepMs != monBucketMs {
		t.Errorf("unprobed inbound = %+v, want an empty series list at the default step", none)
	}
}

// TestUIStatsLongRangeReadsRollup: past 24h the series come from
// mon_stats_rollup at the step configured right now — rows left at a previous
// step are invisible, and so is the fine table — and the 30d window is snapped
// to that step.
func TestUIStatsLongRangeReadsRollup(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true)
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"})
	if err := (&SettingService{}).SetMonRollupStepMinutes(60); err != nil {
		t.Fatalf("set rollup step: %v", err)
	}

	now := monSummaryNow
	const hour = int64(3600000)
	from := now.Add(-7 * 24 * time.Hour).UnixMilli()
	monRollup(t, "ams-1", "xray", 1, "direct", from, hour, 1, 12, 110, 10)
	monRollup(t, "ams-1", "xray", 1, "direct", from+167*hour, hour, 1, 12, 120, 0)
	// Leftovers of a previous monRollupStepMinutes, and the fine table.
	monRollup(t, "ams-1", "xray", 1, "direct", from+hour, hour/2, 1, 6, 999, 999)
	monCurrentLat(t, "ams-1", "xray", 1, "direct", from+2*hour, 1, 999, 999, 999)

	out, err := m.UIStats(now, "xray", 1, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("UIStats 7d: %v", err)
	}
	if out.Source != MonUISourceRollup || out.StepMs != hour {
		t.Errorf("source = %s step = %d, want rollup at %d", out.Source, out.StepMs, hour)
	}
	if out.From != from || out.To != now.UnixMilli() {
		t.Errorf("window = [%d,%d), want [%d,%d)", out.From, out.To, from, now.UnixMilli())
	}
	if len(out.Series) != 1 {
		t.Fatalf("series = %+v, want one", out.Series)
	}
	points := out.Series[0].Points
	if len(points) != 168 {
		t.Fatalf("points = %d, want 168 hours", len(points))
	}
	for i, p := range points {
		filled := i == 0 || i == 167
		if filled != (p != nil) {
			t.Errorf("point %d = %v, want filled=%v — the old step and the fine table are not drawn", i, p, filled)
		}
	}
	if points[0].NOk != 110 || points[0].NFail != 10 || points[167].NOk != 120 {
		t.Errorf("points = %+v / %+v, want the two hourly rollup buckets", points[0], points[167])
	}

	month, err := m.UIStats(now, "xray", 1, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("UIStats 30d: %v", err)
	}
	if month.From != now.Add(-30*24*time.Hour).UnixMilli() || month.To != now.UnixMilli() {
		t.Errorf("30d window = [%d,%d), want [%d,%d)", month.From, month.To,
			now.Add(-30*24*time.Hour).UnixMilli(), now.UnixMilli())
	}
	if len(month.Series[0].Points) != 720 {
		t.Errorf("30d points = %d, want 720 hours", len(month.Series[0].Points))
	}
}

// TestUIStatsSnapsAnUnalignedClockToTheStep: the bucket in progress is left
// out, so the page never draws the current partial bucket as a dip.
func TestUIStatsSnapsAnUnalignedClockToTheStep(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true)

	now := monSummaryNow.Add(137 * time.Second)
	out, err := m.UIStats(now, "xray", 1, time.Hour)
	if err != nil {
		t.Fatalf("UIStats: %v", err)
	}
	if out.To != monSummaryNow.UnixMilli() || out.From != monSummaryNow.Add(-time.Hour).UnixMilli() {
		t.Errorf("window = [%d,%d), want it snapped to [%d,%d)", out.From, out.To,
			monSummaryNow.Add(-time.Hour).UnixMilli(), monSummaryNow.UnixMilli())
	}
	if (out.To-out.From)/out.StepMs != 12 {
		t.Errorf("window holds %d steps, want 12", (out.To-out.From)/out.StepMs)
	}
}
