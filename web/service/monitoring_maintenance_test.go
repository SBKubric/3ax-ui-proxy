package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

type staleRecorder struct {
	stale []time.Time
	back  []time.Duration
}

func (r *staleRecorder) NotifyMonitoringStale(since time.Time) { r.stale = append(r.stale, since) }
func (r *staleRecorder) NotifyMonitoringBack(silentFor time.Duration) {
	r.back = append(r.back, silentFor)
}

func newMaintenanceTestService(t *testing.T) *MonitoringService {
	t.Helper()
	m := newMonitoringTestService(t)
	resetMonContactForTest()
	resetMonStaleForTest()
	t.Cleanup(func() { resetMonContactForTest(); resetMonStaleForTest() })
	return m
}

// TestStaleEdges: never before the first contact; once, after the threshold;
// cleared by the next authorised request with the silence's length; and not
// re-announced while it lasts.
func TestStaleEdges(t *testing.T) {
	m := newMaintenanceTestService(t)
	rec := &staleRecorder{}
	SetMonStaleNotifier(rec)
	t0 := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	if m.CheckMonStale(t0.Add(24 * time.Hour)) {
		t.Error("STALE declared before mon-server ever reached the panel")
	}
	m.TouchMonLastContact(t0)
	if m.CheckMonStale(t0.Add(15 * time.Minute)) {
		t.Error("STALE declared at exactly the threshold")
	}
	if !m.CheckMonStale(t0.Add(16 * time.Minute)) {
		t.Fatal("STALE not declared past the threshold")
	}
	if stale, since := m.IsMonStale(); !stale || since != t0.UnixMilli() {
		t.Errorf("IsMonStale = %v, %d; want true, %d", stale, since, t0.UnixMilli())
	}
	if m.CheckMonStale(t0.Add(30 * time.Minute)) {
		t.Error("STALE announced twice")
	}
	if len(rec.stale) != 1 || !rec.stale[0].Equal(t0) || len(rec.back) != 0 {
		t.Errorf("notifier: stale=%v back=%v", rec.stale, rec.back)
	}

	m.TouchMonLastContact(t0.Add(40 * time.Minute))
	if stale, _ := m.IsMonStale(); stale {
		t.Error("an authorised request did not clear STALE")
	}
	if len(rec.back) != 1 || rec.back[0] != 40*time.Minute {
		t.Errorf("back notification: %v, want one of 40m", rec.back)
	}
	// A restart reads the last contact from the setting and can still go STALE.
	resetMonContactForTest()
	if !m.CheckMonStale(t0.Add(2 * time.Hour)) {
		t.Error("after a restart the persisted last contact did not lead to STALE")
	}
	// The threshold is a setting.
	resetMonStaleForTest()
	SetMonStaleNotifier(rec)
	setSetting(t, "monStaleMinutes", "5")
	m.TouchMonLastContact(t0.Add(3 * time.Hour))
	if !m.CheckMonStale(t0.Add(3*time.Hour + 6*time.Minute)) {
		t.Error("a 5-minute threshold was not honoured")
	}
}

// TestProbeSetTTL: an ensured set survives inside the TTL and goes after it;
// stray probes without a subId are swept; nothing to do otherwise.
func TestProbeSetTTL(t *testing.T) {
	m := newMaintenanceTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	now := time.Now()

	if removed, err := m.ExpireProbeSet(now); err != nil || removed {
		t.Errorf("nothing to expire: removed=%v err=%v", removed, err)
	}
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.ExpireProbeSet(now.Add(23 * time.Hour)); err != nil || removed {
		t.Errorf("inside the TTL: removed=%v err=%v", removed, err)
	}
	if _, ok := probeEmails(t, 1)["probe-1"]; !ok {
		t.Fatal("probe gone inside the TTL")
	}
	if removed, err := m.ExpireProbeSet(now.Add(25 * time.Hour)); err != nil || !removed {
		t.Errorf("past the TTL: removed=%v err=%v", removed, err)
	}
	if _, ok := probeEmails(t, 1)["probe-1"]; ok {
		t.Error("probe still there past the TTL")
	}
	if subId, _ := (&SettingService{}).GetMonProbeSubId(); subId != "" {
		t.Errorf("subId %q kept after expiry", subId)
	}

	// A probe left behind with no subId (a reset by hand) is swept too.
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	setSetting(t, "monProbeSubId", "")
	setSetting(t, "monProbeLastEnsured", "0")
	if removed, err := m.ExpireProbeSet(now); err != nil || !removed {
		t.Errorf("sweep without subId: removed=%v err=%v", removed, err)
	}
	if _, ok := probeEmails(t, 1)["probe-1"]; ok {
		t.Error("stray probe not swept")
	}
	setSetting(t, "monProbeTtlHours", "1")
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if removed, _ := m.ExpireProbeSet(now.Add(2 * time.Hour)); !removed {
		t.Error("a 1-hour TTL was not honoured")
	}
}

// TestRetentionPrunesInBatches: only rows past their table's window go, and
// a table larger than one batch is drained batch by batch.
func TestRetentionPrunesInBatches(t *testing.T) {
	m := newMaintenanceTestService(t)
	db := database.GetDB()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).UnixMilli()
	fresh := now.Add(-time.Hour).UnixMilli()
	oldRollup := now.Add(-31 * 24 * time.Hour).UnixMilli()
	keptRollup := now.Add(-20 * 24 * time.Hour).UnixMilli()

	events := make([]model.MonEvent, 0, monPruneBatch+4)
	for i := 0; i < monPruneBatch+1; i++ {
		events = append(events, model.MonEvent{Id: uuidAt(i), Ts: old, ReceivedAt: old, Kind: "panel", To: "PANEL_DOWN"})
	}
	for i := 0; i < 3; i++ {
		events = append(events, model.MonEvent{Id: uuidAt(monPruneBatch + 1 + i), Ts: fresh, ReceivedAt: fresh, Kind: "panel", To: "PANEL_UP"})
	}
	if err := db.CreateInBatches(events, 500).Error; err != nil {
		t.Fatal(err)
	}
	for _, b := range []int64{old - old%monBucketMs, fresh - fresh%monBucketMs} {
		if err := db.Create(&model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "proxy", BucketStart: b, BucketMs: monBucketMs}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []int64{oldRollup, keptRollup} {
		if err := db.Create(&model.MonStatsRollup{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "proxy", StepMs: 3600000, BucketStart: b - b%3600000}).Error; err != nil {
			t.Fatal(err)
		}
	}

	removed, err := m.PruneRetention(now)
	if err != nil {
		t.Fatalf("PruneRetention: %v", err)
	}
	if removed["mon_events"] != int64(monPruneBatch+1) || removed["mon_stats_current"] != 1 || removed["mon_stats_rollup"] != 1 {
		t.Errorf("removed %v", removed)
	}
	count := func(table string) int64 {
		var n int64
		db.Table(table).Count(&n)
		return n
	}
	if count("mon_events") != 3 || count("mon_stats_current") != 1 || count("mon_stats_rollup") != 1 {
		t.Errorf("left events=%d current=%d rollup=%d, want 3/1/1", count("mon_events"), count("mon_stats_current"), count("mon_stats_rollup"))
	}
}

func uuidAt(i int) string {
	const hex = "0123456789abcdef"
	b := []byte("019254a0-0000-7000-8000-000000000000")
	for p := len(b) - 1; i > 0 && p >= 24; p-- {
		b[p] = hex[i%16]
		i /= 16
	}
	return string(b)
}

// TestRollupRebuildAfterStepChange: with a new step every current bucket in
// the window is folded at the new width, rows of the old step stay, and a
// second run finds nothing to do.
func TestRollupRebuildAfterStepChange(t *testing.T) {
	m := newMaintenanceTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	db := database.GetDB()
	now := time.Now()
	hour := (now.Add(-2*time.Hour).UnixMilli() / 3600000) * 3600000

	var batch []MonStatIn
	for i := int64(0); i < 24; i++ { // two hours of 5-minute buckets
		batch = append(batch, stat("ams-1", 1, "proxy", hour+i*monBucketMs, 1, 0, i64(10), i64(10), i64(10)))
	}
	if _, err := m.UpsertStats(batch); err != nil {
		t.Fatal(err)
	}
	var hourly int64
	db.Model(&model.MonStatsRollup{}).Where("step_ms = ?", 3600000).Count(&hourly)
	if hourly != 2 {
		t.Fatalf("hourly rollup rows = %d, want 2", hourly)
	}
	if n, err := m.RebuildMissingRollup(now); err != nil || n != 0 {
		t.Errorf("nothing missing at the current step: rebuilt %d, %v", n, err)
	}

	setSetting(t, "monRollupStepMinutes", "30")
	n, err := m.RebuildMissingRollup(now)
	if err != nil || n != 4 {
		t.Fatalf("rebuild at 30 min: %d, %v; want 4", n, err)
	}
	var halves []model.MonStatsRollup
	db.Where("step_ms = ?", 1800000).Order("bucket_start").Find(&halves)
	if len(halves) != 4 {
		t.Fatalf("30-min rows = %d", len(halves))
	}
	for _, h := range halves {
		if h.NBuckets != 6 || h.NOk != 6 || h.LatAvg == nil || *h.LatAvg != 10 {
			t.Errorf("30-min row %+v", h)
		}
	}
	db.Model(&model.MonStatsRollup{}).Where("step_ms = ?", 3600000).Count(&hourly)
	if hourly != 2 {
		t.Errorf("old-step rows = %d after the rebuild, want 2 (they age out under retention)", hourly)
	}
	if n, err := m.RebuildMissingRollup(now); err != nil || n != 0 {
		t.Errorf("second run rebuilt %d, %v; want 0", n, err)
	}
}
