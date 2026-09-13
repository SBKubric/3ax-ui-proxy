package service

import (
	"errors"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/google/uuid"
)

func v7(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

func targetEvent(t *testing.T, ts int64, monClient string, inboundId int, from, to, reason string) MonEventIn {
	return MonEventIn{Id: v7(t), Ts: ts, Kind: model.MonEventTarget, MonClientId: monClient, InboundKind: model.MonKindXray,
		InboundId: inboundId, Path: model.MonPathProxy, From: from, To: to, Reason: reason}
}

func loadTarget(t *testing.T, monClient string, inboundId int, path string) *model.MonTarget {
	t.Helper()
	var row model.MonTarget
	err := database.GetDB().Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ?",
		monClient, model.MonKindXray, inboundId, path).First(&row).Error
	if database.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &row
}

// TestApplyEventsFeedAndState: duplicates are counted not stored, unknown
// inbounds are ignored, an older event goes to the feed without rolling the
// state back, mon_client and panel events touch no target, and only the
// events with notified=false reach the notifier.
func TestApplyEventsFeedAndState(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	ib := seedXrayInbound(t, is, "reality", true)
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", Name: "Amsterdam #1", Region: "NL", State: "ONLINE"}}); err != nil {
		t.Fatal(err)
	}
	var notified []MonNotification
	SetMonEventNotifier(func(n []MonNotification) { notified = append(notified, n...) })
	t.Cleanup(func() { SetMonEventNotifier(nil) })

	down := targetEvent(t, 2_000, "ams-1", ib.Id, "UP", "DOWN", "tls_timeout")
	offline := MonEventIn{Id: v7(t), Ts: 3_000, Kind: model.MonEventMonClient, MonClientId: "msk-1", From: "ONLINE", To: "OFFLINE", Reason: "heartbeat_missed"}
	panelDown := MonEventIn{Id: v7(t), Ts: 500, Kind: model.MonEventPanel, From: "PANEL_UP", To: "PANEL_DOWN", Reason: "http_timeout"}
	unknown := targetEvent(t, 4_000, "ams-1", 9999, "UP", "DOWN", "tcp_refused")

	res, err := m.ApplyEvents(&MonEventsBatch{Events: []MonEventIn{down, offline, panelDown, unknown}})
	if err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	if res.Accepted != 3 || res.Duplicates != 0 || len(res.Ignored) != 1 || res.Ignored[0].Id != unknown.Id || res.Ignored[0].Error != "unknown_inbound" {
		t.Fatalf("result: %+v", res)
	}
	target := loadTarget(t, "ams-1", ib.Id, model.MonPathProxy)
	if target == nil || target.State != "DOWN" || target.Since != 2_000 || target.Reason != "tls_timeout" {
		t.Fatalf("DOWN not applied: %+v", target)
	}

	// A later batch brings an event that happened before the DOWN (a backlog
	// after PANEL_DOWN): it goes to the feed, the state stays.
	late := targetEvent(t, 1_000, "ams-1", ib.Id, "UNKNOWN", "UP", "recovered")
	late.Notified = true
	if res, err := m.ApplyEvents(&MonEventsBatch{Events: []MonEventIn{late}}); err != nil || res.Accepted != 1 {
		t.Fatalf("late event: %v %+v", err, res)
	}
	target = loadTarget(t, "ams-1", ib.Id, model.MonPathProxy)
	if target.State != "DOWN" || target.Since != 2_000 {
		t.Fatalf("older UP must not roll DOWN back: %+v", target)
	}
	var events []model.MonEvent
	database.GetDB().Order("ts asc").Find(&events)
	if len(events) != 4 {
		t.Fatalf("feed has %d events, want 4 (unknown inbound not stored): %+v", len(events), events)
	}
	if events[1].Id != late.Id {
		t.Errorf("late event missing from the feed: %+v", events)
	}
	if events[0].Kind != "panel" || !events[0].Notified {
		t.Errorf("panel event must be stored as notified: %+v", events[0])
	}
	if events[0].ReceivedAt == 0 {
		t.Errorf("receivedAt not set")
	}
	if loadTarget(t, "msk-1", ib.Id, model.MonPathProxy) != nil {
		t.Errorf("a mon_client event created a target row")
	}
	if len(notified) != 2 {
		t.Fatalf("notifier got %d events, want DOWN and OFFLINE only: %+v", len(notified), notified)
	}
	if notified[0].Event.Id != down.Id || notified[0].InboundRemark != "reality" || notified[0].MonClient.Name != "Amsterdam #1" ||
		notified[0].MonClient.Region != "NL" || notified[0].PrevState != "" {
		t.Errorf("DOWN notification context: %+v", notified[0])
	}
	if notified[1].Event.Id != offline.Id || notified[1].MonClient.Id != "msk-1" {
		t.Errorf("OFFLINE notification: %+v", notified[1])
	}

	// Duplicate ids are a 200 and no-ops; an UP after the DOWN carries the
	// previous state to the notifier so it can say how long it was down.
	notified = nil
	up := targetEvent(t, 5_000, "ams-1", ib.Id, "DOWN", "UP", "recovered")
	res, err = m.ApplyEvents(&MonEventsBatch{Events: []MonEventIn{down, up}})
	if err != nil {
		t.Fatalf("ApplyEvents with a duplicate: %v", err)
	}
	if res.Accepted != 1 || res.Duplicates != 1 {
		t.Fatalf("duplicate handling: %+v", res)
	}
	target = loadTarget(t, "ams-1", ib.Id, model.MonPathProxy)
	if target.State != "UP" || target.Since != 5_000 {
		t.Errorf("UP not applied: %+v", target)
	}
	if len(notified) != 1 || notified[0].PrevState != "DOWN" || notified[0].PrevSince != 2_000 {
		t.Errorf("UP notification context: %+v", notified)
	}
}

// TestApplyEventsValidatesTheWholeBatch: one bad element fails the batch with
// a 400 naming it, and nothing is stored; over the size limit is a 413.
func TestApplyEventsValidatesTheWholeBatch(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	good := targetEvent(t, 1_000, "ams-1", ib.Id, "UP", "DOWN", "tls_timeout")
	bad := targetEvent(t, 2_000, "ams-1", ib.Id, "UP", "BROKEN", "")
	_, err := m.ApplyEvents(&MonEventsBatch{Events: []MonEventIn{good, bad}})
	var monErr *MonError
	if !errors.As(err, &monErr) || monErr.Status != 400 || monErr.Code != "invalid_body" || monErr.Message != `events[1].to: unknown value "BROKEN"` {
		t.Fatalf("want 400 invalid_body naming events[1].to, got %v", err)
	}
	var n int64
	database.GetDB().Model(&model.MonEvent{}).Count(&n)
	if n != 0 {
		t.Errorf("a rejected batch stored %d events", n)
	}

	cases := []struct {
		name string
		mut  func(*MonEventIn)
	}{
		{"uppercase id", func(e *MonEventIn) { e.Id = "019254A0-7C3E-7D2A-9B4F-1F2E3D4C5B6A" }},
		{"not a uuid", func(e *MonEventIn) { e.Id = "not-a-uuid-at-all-not-a-uuid-at-all-" }},
		{"kind", func(e *MonEventIn) { e.Kind = "foo" }},
		{"path", func(e *MonEventIn) { e.Path = "sideways" }},
		{"inboundKind", func(e *MonEventIn) { e.InboundKind = "wg" }},
		{"monClientId", func(e *MonEventIn) { e.MonClientId = "has space" }},
		{"ts", func(e *MonEventIn) { e.Ts = 0 }},
	}
	for _, c := range cases {
		e := good
		c.mut(&e)
		_, err := m.ApplyEvents(&MonEventsBatch{Events: []MonEventIn{e}})
		if !errors.As(err, &monErr) || monErr.Status != 400 {
			t.Errorf("%s: want 400, got %v", c.name, err)
		}
	}

	many := make([]MonEventIn, MonMaxEvents+1)
	for i := range many {
		many[i] = good
	}
	_, err = m.ApplyEvents(&MonEventsBatch{Events: many})
	if !errors.As(err, &monErr) || monErr.Status != 413 || monErr.Code != "batch_too_large" {
		t.Fatalf("want 413 batch_too_large, got %v", err)
	}
}

func stat(monClient string, inboundId int, bucketStart int64, nOk, nFail int, avg *int64) MonStatIn {
	s := MonStatIn{MonClientId: monClient, InboundKind: model.MonKindXray, InboundId: inboundId, Path: model.MonPathDirect,
		BucketStart: bucketStart, NOk: nOk, NFail: nFail, LatencyAvgMs: avg}
	if avg != nil {
		lo, hi := *avg-10, *avg+10
		s.LatencyMinMs, s.LatencyMaxMs = &lo, &hi
	}
	return s
}

func i64(v int64) *int64 { return &v }

// TestUpsertStatsAndRollup: an aggregate with a known key replaces the stored
// one; the rollup of the hour is recomputed in the same call with n_buckets
// showing how many 5-minute rows went in and lat_avg weighted by n_ok; an
// unknown inbound and a bucket older than the retention are ignored; a
// series' first aggregate creates its target row as UNKNOWN.
func TestUpsertStatsAndRollup(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	db := database.GetDB()

	hour := time.Now().Add(-2 * time.Hour).UnixMilli()
	hour -= hour % 3_600_000
	b1, b2 := hour, hour+300_000
	old := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	old -= old % 300_000

	res, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{
		stat("ams-1", ib.Id, b1, 5, 0, i64(40)),
		stat("ams-1", ib.Id, b2, 15, 5, i64(60)),
		stat("ams-1", 9999, b1, 1, 0, i64(20)),
		stat("ams-1", ib.Id, old, 1, 0, i64(20)),
	}})
	if err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}
	if res.Accepted != 2 || len(res.Ignored) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if res.Ignored[0].InboundId != 9999 || res.Ignored[0].Error != "unknown_inbound" || res.Ignored[1].BucketStart != old || res.Ignored[1].Error != "too_old" {
		t.Errorf("ignored: %+v", res.Ignored)
	}
	var current []model.MonStatsCurrent
	db.Order("bucket_start asc").Find(&current)
	if len(current) != 2 || current[0].BucketMs != 300_000 || *current[0].LatAvg != 40 {
		t.Fatalf("current rows: %+v", current)
	}
	var rollup []model.MonStatsRollup
	db.Find(&rollup)
	if len(rollup) != 1 {
		t.Fatalf("rollup rows: %+v", rollup)
	}
	r := rollup[0]
	if r.StepMs != 3_600_000 || r.BucketStart != hour || r.NBuckets != 2 || r.NOk != 20 || r.NFail != 5 {
		t.Errorf("rollup counts: %+v", r)
	}
	if r.LatAvg == nil || *r.LatAvg != 55 { // (5*40 + 15*60) / 20
		t.Errorf("rollup lat_avg should be weighted by n_ok (55), got %v", r.LatAvg)
	}
	if r.LatMin == nil || *r.LatMin != 30 || r.LatMax == nil || *r.LatMax != 70 {
		t.Errorf("rollup lat extremes: min %v max %v", r.LatMin, r.LatMax)
	}
	if target := loadTarget(t, "ams-1", ib.Id, model.MonPathDirect); target == nil || target.State != "UNKNOWN" {
		t.Errorf("first aggregate must create the target row as UNKNOWN: %+v", target)
	}

	// Same key again: the row is replaced and the rollup follows.
	res, err = m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{stat("ams-1", ib.Id, b2, 0, 20, nil)}})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("second upsert: %v %+v", err, res)
	}
	db.Order("bucket_start asc").Find(&current)
	if len(current) != 2 || current[1].NOk != 0 || current[1].NFail != 20 || current[1].LatAvg != nil {
		t.Fatalf("upsert did not replace the row: %+v", current)
	}
	db.Find(&rollup)
	if len(rollup) != 1 || rollup[0].NBuckets != 2 || rollup[0].NOk != 5 || rollup[0].NFail != 20 || *rollup[0].LatAvg != 40 {
		t.Errorf("rollup after replace: %+v", rollup)
	}

	// A latency with nOk=0 is meaningless and stored as NULL.
	res, err = m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{stat("ams-1", ib.Id, b1, 0, 3, i64(99))}})
	if err != nil || res.Accepted != 1 {
		t.Fatal(err)
	}
	var first model.MonStatsCurrent
	db.Where("bucket_start = ?", b1).First(&first)
	if first.LatAvg != nil || first.LatMin != nil {
		t.Errorf("latency kept with nOk=0: %+v", first)
	}
}

// TestUpsertStatsValidation: bucketStart off the grid, negative counts and
// an oversized batch are refused before anything is written.
func TestUpsertStatsValidation(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	now := time.Now().UnixMilli()
	var monErr *MonError
	_, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{stat("ams-1", ib.Id, now-now%300_000+7, 1, 0, nil)}})
	if !errors.As(err, &monErr) || monErr.Status != 400 {
		t.Errorf("off-grid bucketStart: want 400, got %v", err)
	}
	_, err = m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{stat("ams-1", ib.Id, now-now%300_000, -1, 0, nil)}})
	if !errors.As(err, &monErr) || monErr.Status != 400 {
		t.Errorf("negative nOk: want 400, got %v", err)
	}
	many := make([]MonStatIn, MonMaxStats+1)
	for i := range many {
		many[i] = stat("ams-1", ib.Id, now-now%300_000, 1, 0, nil)
	}
	_, err = m.UpsertStats(&MonStatsBatch{Stats: many})
	if !errors.As(err, &monErr) || monErr.Status != 413 {
		t.Errorf("oversized batch: want 413, got %v", err)
	}
	var n int64
	database.GetDB().Model(&model.MonStatsCurrent{}).Count(&n)
	if n != 0 {
		t.Errorf("rejected batches stored %d rows", n)
	}
}

// TestRebuildRollupWithNewStep: changing the step rebuilds the rollup with
// the new width; the old step's rows stay.
func TestRebuildRollupWithNewStep(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	db := database.GetDB()
	hour := time.Now().Add(-3 * time.Hour).UnixMilli()
	hour -= hour % 3_600_000
	if _, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{
		stat("ams-1", ib.Id, hour, 5, 0, i64(40)),
		stat("ams-1", ib.Id, hour+1_800_000, 5, 5, i64(60)),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildRollup(1_800_000, 0); err != nil {
		t.Fatalf("RebuildRollup: %v", err)
	}
	var rollup []model.MonStatsRollup
	db.Order("step_ms asc, bucket_start asc").Find(&rollup)
	if len(rollup) != 3 {
		t.Fatalf("want 2 half-hour rows plus the old hour row, got %+v", rollup)
	}
	if rollup[0].StepMs != 1_800_000 || rollup[0].NBuckets != 1 || *rollup[0].LatAvg != 40 ||
		rollup[1].StepMs != 1_800_000 || rollup[1].NBuckets != 1 || *rollup[1].LatAvg != 60 ||
		rollup[2].StepMs != 3_600_000 || rollup[2].NBuckets != 2 {
		t.Errorf("rebuilt rollup: %+v", rollup)
	}
}

// TestWorstLiveTargetState: the badge takes the worst state by priority over
// the mon-clients in the snapshot only; mon-clients that left the registry do
// not count, and an inbound without live targets folds to nothing.
func TestWorstLiveTargetState(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	db := database.GetDB()
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}, {Id: "msk-1"}}); err != nil {
		t.Fatal(err)
	}
	rows := []model.MonTarget{
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct", State: "UP"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "PAUSED"},
		{MonClientId: "msk-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "FLAPPING"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 2, Path: "direct", State: "UP"},
		{MonClientId: "old-1", InboundKind: "xray", InboundId: 2, Path: "direct", State: "DOWN"}, // not in the snapshot
		{MonClientId: "msk-1", InboundKind: "awg", InboundId: 0, Path: "direct", State: "UNKNOWN"},
		{MonClientId: "ams-1", InboundKind: "awg", InboundId: 0, Path: "direct", State: "DOWN"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 3, Path: "direct", State: "PAUSED"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	want := map[MonInboundRef]string{
		{Kind: "xray", InboundId: 1}: "FLAPPING",
		{Kind: "xray", InboundId: 2}: "UP",
		{Kind: "awg", InboundId: 0}:  "DOWN",
		{Kind: "xray", InboundId: 3}: "PAUSED",
		{Kind: "xray", InboundId: 4}: "",
	}
	for ref, state := range want {
		if got := m.WorstLiveTargetState(ref.Kind, ref.InboundId); got != state {
			t.Errorf("WorstLiveTargetState(%s, %d) = %q, want %q", ref.Kind, ref.InboundId, got, state)
		}
	}
	if worstState("UP", "DOWN") != "DOWN" || worstState("PAUSED", "UNKNOWN") != "UNKNOWN" || worstState("", "UP") != "UP" || worstState("FLAPPING", "DOWN") != "DOWN" {
		t.Errorf("worstState priority is off")
	}
}
