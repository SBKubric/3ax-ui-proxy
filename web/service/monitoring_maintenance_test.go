package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestCheckStaleRaisesOnceAndClearsOnContact: STALE is declared when the
// silence exceeds the threshold, once (the hook fires once), never before
// the first contact, and the next authorized contact clears it and fires
// the back hook with the silence's start.
func TestCheckStaleRaisesOnceAndClearsOnContact(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	var staleSince, backSince, backNow int64
	SetMonStatusHooks(MonStatusHooks{
		Stale: func(since int64) { staleSince = since },
		Back:  func(since, now int64) { backSince, backNow = since, now },
	})
	t.Cleanup(func() { SetMonStatusHooks(MonStatusHooks{}) })
	now := time.UnixMilli(1_757_721_600_000)

	if m.CheckStale(now) {
		t.Fatal("STALE declared before mon-server ever called")
	}
	m.TouchLastContact(now.Add(-10 * time.Minute))
	if m.CheckStale(now) {
		t.Fatal("STALE declared under the 15-minute threshold")
	}
	m.TouchLastContact(now.Add(-20 * time.Minute))
	if !m.CheckStale(now) {
		t.Fatal("STALE not declared after 20 minutes of silence")
	}
	if stale, since := m.Stale(); !stale || since != now.Add(-20*time.Minute).UnixMilli() || staleSince != since {
		t.Fatalf("stale flag: %v since %d, hook got %d", stale, since, staleSince)
	}
	if m.CheckStale(now.Add(time.Minute)) {
		t.Fatal("STALE announced twice")
	}

	m.RecordContact(now.Add(2 * time.Minute))
	if stale, _ := m.Stale(); stale {
		t.Fatal("contact did not clear STALE")
	}
	if backSince != now.Add(-20*time.Minute).UnixMilli() || backNow != now.Add(2*time.Minute).UnixMilli() {
		t.Errorf("back hook got since=%d now=%d", backSince, backNow)
	}
	if m.CheckStale(now.Add(3 * time.Minute)) {
		t.Fatal("STALE right after a contact")
	}

	// A shorter threshold from the settings is honoured.
	if err := m.settingService.SetMonStaleMinutes(1); err != nil {
		t.Fatal(err)
	}
	if !m.CheckStale(now.Add(5 * time.Minute)) {
		t.Fatal("custom threshold ignored")
	}
}

// TestSweepProbeTTL: the set goes after monProbeTtlHours without an ensure
// (and the subId with it), stays while ensures are fresh, and stray probe
// accounts without a set are cleaned up too.
func TestSweepProbeTTL(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	live := seedXrayInbound(t, is, "live", true)
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	if removed, err := m.SweepProbeTTL(now.Add(time.Hour)); err != nil || removed {
		t.Fatalf("fresh set removed: %v %v", removed, err)
	}
	if removed, err := m.SweepProbeTTL(now.Add(25 * time.Hour)); err != nil || !removed {
		t.Fatalf("set older than 24h not removed: %v %v", removed, err)
	}
	if got := probeEmails(t, is, live.Id); len(got) != 0 {
		t.Errorf("probe account survived the TTL: %v", got)
	}
	if v, _ := m.settingService.GetMonProbeSubId(); v != "" {
		t.Errorf("subId not forgotten: %q", v)
	}

	// Accounts without a set: ensure again, then lose the subId.
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if err := m.settingService.SetMonProbeSubId(""); err != nil {
		t.Fatal(err)
	}
	if removed, err := m.SweepProbeTTL(now); err != nil || !removed {
		t.Fatalf("stray probe accounts not removed: %v %v", removed, err)
	}
	if got := probeEmails(t, is, live.Id); len(got) != 0 {
		t.Errorf("stray probe account survived: %v", got)
	}
	if removed, err := m.SweepProbeTTL(now); err != nil || removed {
		t.Fatalf("nothing left to remove, yet removed=%v err=%v", removed, err)
	}

	// A custom TTL is honoured.
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if err := m.settingService.SetMonProbeTtlHours(2); err != nil {
		t.Fatal(err)
	}
	if removed, _ := m.SweepProbeTTL(now.Add(3 * time.Hour)); !removed {
		t.Error("custom TTL ignored")
	}
}

// TestRetentionDeletesOnlyOldRowsInBatches: rows older than the windows go
// (more than one batch of them), newer rows stay, rollup rows follow their
// own longer retention.
func TestRetentionDeletesOnlyOldRowsInBatches(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	db := database.GetDB()
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour).UnixMilli()
	old -= old % 300_000
	fresh := now.Add(-time.Hour).UnixMilli()
	fresh -= fresh % 300_000

	current := make([]model.MonStatsCurrent, 0, monRetentionBatch+2)
	for i := 0; i < monRetentionBatch+1; i++ {
		current = append(current, model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct",
			BucketStart: old - int64(i)*300_000, BucketMs: 300_000, NOk: 1})
	}
	current = append(current, model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct",
		BucketStart: fresh, BucketMs: 300_000, NOk: 1})
	if err := db.CreateInBatches(current, 500).Error; err != nil {
		t.Fatal(err)
	}
	events := []model.MonEvent{
		{Id: "00000000-0000-7000-8000-000000000001", Ts: old, Kind: "panel", ToState: "PANEL_DOWN"},
		{Id: "00000000-0000-7000-8000-000000000002", Ts: fresh, Kind: "panel", ToState: "PANEL_UP"},
	}
	if err := db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}
	veryOld := now.Add(-31 * 24 * time.Hour).UnixMilli()
	rollup := []model.MonStatsRollup{
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct", StepMs: 3_600_000, BucketStart: veryOld - veryOld%3_600_000, NBuckets: 1},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct", StepMs: 3_600_000, BucketStart: old - old%3_600_000, NBuckets: 1}, // 8 days: fine at 30
	}
	if err := db.Create(&rollup).Error; err != nil {
		t.Fatal(err)
	}

	n, err := m.Retention(now)
	if err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if n != int64(monRetentionBatch+1)+1+1 {
		t.Errorf("removed %d rows, want %d", n, monRetentionBatch+3)
	}
	var left int64
	db.Model(&model.MonStatsCurrent{}).Count(&left)
	if left != 1 {
		t.Errorf("mon_stats_current: %d rows left, want the fresh one", left)
	}
	db.Model(&model.MonEvent{}).Count(&left)
	if left != 1 {
		t.Errorf("mon_events: %d rows left, want the fresh one", left)
	}
	db.Model(&model.MonStatsRollup{}).Count(&left)
	if left != 1 {
		t.Errorf("mon_stats_rollup: %d rows left, want the 8-day one", left)
	}
}

// TestRebuildRollupIfIncomplete: a changed step leaves the window without
// rollup rows of that step, so the sweep rebuilds them; the old step's rows
// stay for their own retention; a complete rollup is left alone.
func TestRebuildRollupIfIncomplete(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	db := database.GetDB()
	now := time.Now()
	hour := now.Add(-3 * time.Hour).UnixMilli()
	hour -= hour % 3_600_000
	if _, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{
		stat("ams-1", ib.Id, hour, 5, 0, i64(40)),
		stat("ams-1", ib.Id, hour+1_800_000, 5, 5, i64(60)),
	}}); err != nil {
		t.Fatal(err)
	}
	if rebuilt, err := m.RebuildRollupIfIncomplete(now); err != nil || rebuilt {
		t.Fatalf("complete rollup rebuilt: %v %v", rebuilt, err)
	}
	if err := m.settingService.SetMonRollupStepMinutes(30); err != nil {
		t.Fatal(err)
	}
	if rebuilt, err := m.RebuildRollupIfIncomplete(now); err != nil || !rebuilt {
		t.Fatalf("step change not noticed: %v %v", rebuilt, err)
	}
	var rows []model.MonStatsRollup
	db.Order("step_ms asc, bucket_start asc").Find(&rows)
	if len(rows) != 3 || rows[0].StepMs != 1_800_000 || rows[1].StepMs != 1_800_000 || rows[2].StepMs != 3_600_000 {
		t.Fatalf("rollup after rebuild: %+v", rows)
	}
	if rebuilt, _ := m.RebuildRollupIfIncomplete(now); rebuilt {
		t.Error("rebuilt again although complete")
	}
}
