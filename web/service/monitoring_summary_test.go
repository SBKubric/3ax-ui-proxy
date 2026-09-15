package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// monSummaryNow is a fixed clock so bucket arithmetic in the fixtures is
// readable.
var monSummaryNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

func monRegister(t *testing.T, clients ...MonClient) {
	t.Helper()
	if err := (&MonitoringService{}).setRegistrySnapshot(clients); err != nil {
		t.Fatalf("set registry snapshot: %v", err)
	}
}

func monRemark(t *testing.T, id int, remark string) {
	t.Helper()
	if err := database.GetDB().Model(&model.Inbound{}).Where("id = ?", id).Update("remark", remark).Error; err != nil {
		t.Fatalf("set remark of inbound %d: %v", id, err)
	}
}

// monCurrent stores n consecutive 5-minute buckets starting at start.
func monCurrent(t *testing.T, client, kind string, id int, path string, start int64, n, ok, fail int) {
	t.Helper()
	db := database.GetDB()
	for i := 0; i < n; i++ {
		row := model.MonStatsCurrent{
			MonClientId: client, InboundKind: kind, InboundId: id, Path: path,
			BucketStart: start + int64(i)*monBucketMs, BucketMs: monBucketMs, NOk: ok, NFail: fail,
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("create current bucket: %v", err)
		}
	}
}

func monRollup(t *testing.T, client, kind string, id int, path string, start, stepMs int64, n, buckets, ok, fail int) {
	t.Helper()
	db := database.GetDB()
	for i := 0; i < n; i++ {
		row := model.MonStatsRollup{
			MonClientId: client, InboundKind: kind, InboundId: id, Path: path,
			StepMs: stepMs, BucketStart: start + int64(i)*stepMs, NBuckets: buckets, NOk: ok, NFail: fail,
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("create rollup bucket: %v", err)
		}
	}
}

func monStoreEvent(t *testing.T, id string, ts int64, kind, client, inboundKind string, inboundId int, path, from, to string) {
	t.Helper()
	ev := model.MonEvent{Id: id, Ts: ts, ReceivedAt: ts, Kind: kind, MonClientId: client,
		InboundKind: inboundKind, InboundId: inboundId, Path: path, From: from, To: to, Notified: true}
	if err := database.GetDB().Create(&ev).Error; err != nil {
		t.Fatalf("create event %s: %v", id, err)
	}
}

func pathByName(t *testing.T, ib MonSummaryInbound, path string) MonSummaryPath {
	t.Helper()
	for _, p := range ib.Paths {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("inbound %s#%d has no %s path (paths: %+v)", ib.Kind, ib.InboundId, path, ib.Paths)
	return MonSummaryPath{}
}

func nearly(got, want float64) bool { return got-want < 1e-9 && want-got < 1e-9 }

// TestSummaryUptimeCoverageAndIncidents walks the whole §7.4 calculation over
// mon_stats_current: uptime folded over mon-clients, coverage against the
// buckets two mon-clients owed, incidents counted from DOWN target events
// only, and the worst target ignoring a target nobody really measured.
func TestSummaryUptimeCoverageAndIncidents(t *testing.T) {
	m := newMonitoringTestService(t)
	t.Cleanup(resetMonStaleForTest)
	monInbound(t, 1, model.VLESS, true)
	monInbound(t, 2, model.VLESS, true)
	monInbound(t, 3, model.VLESS, false)
	monRemark(t, 1, "Reality main")
	monRemark(t, 2, "Reality spare")
	monRegister(t,
		MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"},
		MonClient{Id: "fra-1", Name: "Frankfurt", Region: "eu-central", State: "OFFLINE"},
	)

	from := monSummaryNow.Add(-24 * time.Hour).UnixMilli()
	// inbound 1, direct: one mon-client at 90 %, one at 100 %, each covering
	// half of the window.
	monCurrent(t, "ams-1", "xray", 1, "direct", from, 144, 9, 1)
	monCurrent(t, "fra-1", "xray", 1, "direct", from, 144, 10, 0)
	// inbound 1, proxy: one mon-client covering the whole window.
	monCurrent(t, "ams-1", "xray", 1, "proxy", from, 288, 10, 0)
	// inbound 2: a handful of failing buckets — the worst uptime in the
	// database, but far below the coverage the spec trusts.
	monCurrent(t, "ams-1", "xray", 2, "direct", from, 10, 0, 5)
	// Outside the window: must not be counted anywhere.
	monCurrent(t, "ams-1", "xray", 1, "direct", from-10*monBucketMs, 5, 0, 10)

	monStoreEvent(t, ev1, from+1000, model.MonEventKindTarget, "ams-1", "xray", 1, "direct", "UP", "DOWN")
	monStoreEvent(t, ev2, from+2000, model.MonEventKindTarget, "fra-1", "xray", 1, "proxy", "UP", "DOWN")
	monStoreEvent(t, ev3, from+3000, model.MonEventKindTarget, "ams-1", "xray", 1, "direct", "DOWN", "UP")
	monStoreEvent(t, ev4, from-1000, model.MonEventKindTarget, "ams-1", "xray", 1, "direct", "UP", "DOWN")
	monStoreEvent(t, ev5, from+4000, model.MonEventKindMonClient, "fra-1", "", 0, "", "ONLINE", "OFFLINE")

	sum, err := m.Summary(monSummaryNow, 24*time.Hour)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.From != from || sum.To != monSummaryNow.UnixMilli() {
		t.Errorf("window = [%d,%d), want [%d,%d)", sum.From, sum.To, from, monSummaryNow.UnixMilli())
	}
	if len(sum.Inbounds) != 2 {
		t.Fatalf("inbounds = %d, want 2 (the disabled one is out)", len(sum.Inbounds))
	}

	one := sum.Inbounds[0]
	if one.InboundId != 1 || one.Remark != "Reality main" {
		t.Errorf("first inbound = %+v, want inbound 1 Reality main", one)
	}
	direct := pathByName(t, one, model.MonPathDirect)
	if direct.NOk != 2736 || direct.NFail != 144 {
		t.Errorf("direct sums = %d/%d, want 2736/144", direct.NOk, direct.NFail)
	}
	if direct.Uptime == nil || !nearly(*direct.Uptime, 2736.0/2880.0) {
		t.Errorf("direct uptime = %v, want %v", direct.Uptime, 2736.0/2880.0)
	}
	// Two mon-clients owe 288 buckets each over 24h of 5-minute buckets.
	if direct.Expected != 576 || direct.Received != 288 || !nearly(direct.Coverage, 0.5) {
		t.Errorf("direct coverage = %d/%d = %v, want 288/576 = 0.5", direct.Received, direct.Expected, direct.Coverage)
	}
	proxy := pathByName(t, one, model.MonPathProxy)
	if proxy.Uptime == nil || !nearly(*proxy.Uptime, 1) || !nearly(proxy.Coverage, 0.5) {
		t.Errorf("proxy = %+v, want uptime 1 coverage 0.5", proxy)
	}
	if !nearly(one.Coverage, 0.5) {
		t.Errorf("inbound coverage = %v, want 0.5", one.Coverage)
	}
	if one.Incidents != 2 {
		t.Errorf("incidents = %d, want 2 (DOWN target events inside the window)", one.Incidents)
	}
	if sum.Inbounds[1].Incidents != 0 {
		t.Errorf("inbound 2 incidents = %d, want 0", sum.Inbounds[1].Incidents)
	}

	if sum.Worst == nil {
		t.Fatal("no worst target")
	}
	if sum.Worst.MonClientId != "ams-1" || sum.Worst.InboundId != 1 || sum.Worst.Path != model.MonPathDirect || !nearly(sum.Worst.Uptime, 0.9) {
		t.Errorf("worst = %+v, want ams-1 on inbound 1 direct at 0.9", sum.Worst)
	}
	if sum.Worst.MonClientName != "Amsterdam" || sum.Worst.Region != "eu-west" {
		t.Errorf("worst is not named from the registry: %+v", sum.Worst)
	}

	if len(sum.OfflineClients) != 1 || sum.OfflineClients[0].Id != "fra-1" {
		t.Errorf("offline clients = %+v, want fra-1", sum.OfflineClients)
	}
	if sum.StaleMs != 0 || sum.StaleNow {
		t.Errorf("stale = %d ms / now %v, want 0 / false", sum.StaleMs, sum.StaleNow)
	}
}

// TestSummaryWithoutData: every enabled inbound is still listed, with no
// paths and no worst target, so the digest can print its "no data" line.
func TestSummaryWithoutData(t *testing.T) {
	m := newMonitoringTestService(t)
	t.Cleanup(resetMonStaleForTest)
	monInbound(t, 1, model.VLESS, true)
	monRemark(t, 1, "Reality main")

	sum, err := m.Summary(monSummaryNow, 24*time.Hour)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(sum.Inbounds) != 1 || len(sum.Inbounds[0].Paths) != 0 {
		t.Fatalf("inbounds = %+v, want one inbound with no paths", sum.Inbounds)
	}
	if sum.Inbounds[0].Coverage != 0 || sum.Worst != nil {
		t.Errorf("coverage = %v, worst = %+v, want 0 and nil", sum.Inbounds[0].Coverage, sum.Worst)
	}
	if len(sum.OfflineClients) != 0 {
		t.Errorf("offline clients = %+v, want none", sum.OfflineClients)
	}
}

// TestSummaryLongRangeReadsRollup: beyond 24h the fine table is not touched,
// only rollup rows at the step configured right now, and coverage still
// counts fine buckets through n_buckets.
func TestSummaryLongRangeReadsRollup(t *testing.T) {
	m := newMonitoringTestService(t)
	t.Cleanup(resetMonStaleForTest)
	monInbound(t, 1, model.VLESS, true)
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"})
	if err := (&SettingService{}).SetMonRollupStepMinutes(60); err != nil {
		t.Fatalf("set rollup step: %v", err)
	}

	window := 7 * 24 * time.Hour
	from := monSummaryNow.Add(-window).UnixMilli()
	const hour = int64(3600000)
	// 84 hourly rollup buckets of 12 fine buckets each: half of the 168-hour
	// window, so coverage is 1008/2016 = 0.5.
	monRollup(t, "ams-1", "xray", 1, "direct", from, hour, 84, 12, 9, 1)
	// A leftover series at a different step must be invisible.
	monRollup(t, "ams-1", "xray", 1, "direct", from, hour/2, 84, 6, 0, 100)
	// And so must the fine table.
	monCurrent(t, "ams-1", "xray", 1, "direct", from, 100, 0, 100)

	sum, err := m.Summary(monSummaryNow, window)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	direct := pathByName(t, sum.Inbounds[0], model.MonPathDirect)
	if direct.NOk != 84*9 || direct.NFail != 84*1 {
		t.Errorf("sums = %d/%d, want %d/%d", direct.NOk, direct.NFail, 84*9, 84)
	}
	if direct.Uptime == nil || !nearly(*direct.Uptime, 0.9) {
		t.Errorf("uptime = %v, want 0.9", direct.Uptime)
	}
	if direct.Received != 84*12 || direct.Expected != 2016 || !nearly(direct.Coverage, 0.5) {
		t.Errorf("coverage = %d/%d = %v, want 1008/2016 = 0.5", direct.Received, direct.Expected, direct.Coverage)
	}
}

// TestStaleMsWithin: closed stretches of silence and the one still open are
// both counted, each clipped to the window asked about.
func TestStaleMsWithin(t *testing.T) {
	newMonitoringTestService(t)
	resetMonStaleForTest()
	t.Cleanup(resetMonStaleForTest)
	m := &MonitoringService{}

	now := monSummaryNow
	nowMs := now.UnixMilli()
	minute := int64(60000)

	// A stretch that ended before the window, one inside it, one straddling
	// its start, and the silence going on right now.
	monStale.Lock()
	monStale.intervals = []monStaleInterval{
		{from: nowMs - 200*minute, to: nowMs - 150*minute},
		{from: nowMs - 90*minute, to: nowMs - 80*minute},
		{from: nowMs - 65*minute, to: nowMs - 55*minute},
	}
	monStale.stale, monStale.since = true, nowMs-20*minute
	monStale.Unlock()

	got := m.StaleMsWithin(nowMs-60*minute, nowMs, now)
	want := 5*minute + 20*minute // 5 min of the straddling stretch, 20 min still open
	if got != want {
		t.Errorf("StaleMsWithin = %d ms, want %d ms", got, want)
	}

	if got := m.StaleMsWithin(nowMs-100*minute, nowMs-70*minute, now); got != 10*minute {
		t.Errorf("closed-only window = %d ms, want %d ms", got, 10*minute)
	}
}

// TestStaleIntervalsRecordedOnBothEdges: the job declaring STALE and the
// request clearing it leave a closed stretch the summary can find.
func TestStaleIntervalsRecordedOnBothEdges(t *testing.T) {
	m := newMonitoringTestService(t)
	resetMonContactForTest()
	resetMonStaleForTest()
	t.Cleanup(func() { resetMonContactForTest(); resetMonStaleForTest() })

	start := monSummaryNow.Add(-2 * time.Hour)
	m.TouchMonLastContact(start)
	if !m.CheckMonStale(start.Add(30 * time.Minute)) {
		t.Fatal("30 minutes of silence did not declare STALE")
	}
	if got := m.StaleMsWithin(start.UnixMilli(), monSummaryNow.UnixMilli(), start.Add(30*time.Minute)); got != 30*60000 {
		t.Errorf("open stretch = %d ms, want %d ms", got, 30*60000)
	}
	m.TouchMonLastContact(start.Add(45 * time.Minute))
	if stale, _ := m.IsMonStale(); stale {
		t.Fatal("a contact did not clear STALE")
	}
	// The stretch is closed now: from the last contact to the one that broke
	// the silence.
	if got := m.StaleMsWithin(start.UnixMilli(), monSummaryNow.UnixMilli(), monSummaryNow); got != 45*60000 {
		t.Errorf("closed stretch = %d ms, want %d ms", got, 45*60000)
	}
}
