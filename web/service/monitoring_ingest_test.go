package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

type recordingNotifier struct{ got []model.MonEvent }

func (r *recordingNotifier) NotifyMonitoringEvents(events []model.MonEvent) []string {
	r.got = append(r.got, events...)
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.Id)
	}
	return ids
}

func i64(v int64) *int64 { return &v }

const (
	ev1 = "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a"
	ev2 = "019254a0-8a11-7e30-8c2d-2a3b4c5d6e7f"
	ev3 = "019254a0-9b22-7f41-9d3e-3b4c5d6e7f80"
	ev4 = "019254a0-ac33-7052-8e4f-4c5d6e7f8091"
	ev5 = "019254a0-bd44-7163-9f50-5d6e7f8091a2"
)

func targetEvent(id string, ts int64, from, to, reason string) MonEventIn {
	return MonEventIn{Id: id, Ts: ts, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", InboundId: 1,
		Path: "proxy", From: from, To: to, Reason: reason}
}

// TestApplyEventsRejectsOnlyTheBadElements: a batch is validated element by
// element — the valid ones are applied, each invalid one is named by its
// index in the batch (and its id, when it has one) and nothing about it is
// written. One bad event no longer costs mon-server the whole batch.
func TestApplyEventsRejectsOnlyTheBadElements(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})

	res, err := m.ApplyEvents([]MonEventIn{
		targetEvent(ev1, 1000, "UP", "DOWN", "tls_timeout"),
		{Id: ev2, Ts: 1001, Kind: "foo", To: "DOWN"},
		{Id: ev3, Ts: 1002, Kind: "panel", From: "PANEL_UP", To: "PANEL_DOWN"},
	})
	if err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	if res.Accepted != 2 || len(res.Rejected) != 1 {
		t.Fatalf("result = %+v, want 2 accepted and 1 rejected", res)
	}
	if r := res.Rejected[0]; r.Index != 1 || r.Id != ev2 || !strings.Contains(r.Error, "events[1].kind") {
		t.Errorf("rejected = %+v, want index 1, id %s, an error naming events[1].kind", r, ev2)
	}
	var stored []string
	database.GetDB().Model(&model.MonEvent{}).Order("ts").Pluck("id", &stored)
	if len(stored) != 2 || stored[0] != ev1 || stored[1] != ev3 {
		t.Errorf("stored events = %v, want [%s %s]", stored, ev1, ev3)
	}

	for _, tc := range []MonEventIn{
		{Id: "not-a-uuid", Ts: 1, Kind: "panel", To: "PANEL_DOWN"},
		{Id: strings.ToUpper(ev4), Ts: 1, Kind: "panel", To: "PANEL_DOWN"},
		{Id: ev4, Ts: 1, Kind: "target", MonClientId: "ams 1", InboundKind: "xray", Path: "proxy", To: "DOWN"},
		{Id: ev4, Ts: 1, Kind: "target", MonClientId: "ams-1", InboundKind: "wg", Path: "proxy", To: "DOWN"},
		{Id: ev4, Ts: 1, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", Path: "tunnel", To: "DOWN"},
		{Id: ev4, Ts: 1, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", Path: "edge:", To: "DOWN"},
		{Id: ev4, Ts: 1, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", Path: "proxy", To: "ONLINE"},
		{Id: ev4, Ts: 1, Kind: "mon_client", To: "OFFLINE"},
		{Id: ev4, Ts: 1, Kind: "mon_client", MonClientId: "msk-1", From: "NEVER", To: "ONLINE"},
		{Id: ev4, Ts: 1, Kind: "panel", To: "DOWN"},
	} {
		res, err := m.ApplyEvents([]MonEventIn{tc})
		if err != nil || res.Accepted != 0 || len(res.Rejected) != 1 || res.Rejected[0].Index != 0 {
			t.Errorf("%+v: res=%+v err=%v, want it rejected at index 0", tc, res, err)
		}
	}
	var n int64
	database.GetDB().Model(&model.MonEvent{}).Where("id = ?", ev4).Count(&n)
	if n != 0 {
		t.Errorf("a rejected event was written")
	}
}

// TestApplyEventsFromRaw: an element that does not even decode into an
// event — a string where a number belongs — is rejected on its own, with the
// id recovered when the element carries one; unknown fields are ignored.
func TestApplyEventsFromRaw(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})

	res, err := m.ApplyEventsRaw([]json.RawMessage{
		json.RawMessage(`{"id":"` + ev1 + `","ts":"soon","kind":"panel","to":"PANEL_DOWN"}`),
		json.RawMessage(`{"id":"` + ev2 + `","ts":1000,"kind":"target","monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"edge:ams-2","from":"","to":"DOWN","reason":"tcp_refused","notified":true,"hopRole":"edge"}`),
		json.RawMessage(`42`),
	})
	if err != nil {
		t.Fatalf("ApplyEventsRaw: %v", err)
	}
	if res.Accepted != 1 || len(res.Rejected) != 2 {
		t.Fatalf("result = %+v, want 1 accepted and 2 rejected", res)
	}
	if r := res.Rejected[0]; r.Index != 0 || r.Id != ev1 || !strings.Contains(r.Error, "events[0]") {
		t.Errorf("rejected[0] = %+v, want index 0 with id %s", r, ev1)
	}
	if r := res.Rejected[1]; r.Index != 2 || r.Id != "" {
		t.Errorf("rejected[1] = %+v, want index 2 without an id", r)
	}
	var target model.MonTarget
	if err := database.GetDB().Where("path = ?", "edge:ams-2").First(&target).Error; err != nil || target.State != "DOWN" {
		t.Errorf("edge target = %+v, %v; want a DOWN row under path edge:ams-2", target, err)
	}
}

// TestMonPathGrammar: direct | proxy | edge:<hop name> | inner:<hop name>,
// the hop name by the chain registry's own rule; nothing else.
func TestMonPathGrammar(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"direct", true},
		{"proxy", true},
		{"edge:ams-1", true},
		{"inner:core-1", true},
		{"edge:" + strings.Repeat("a", 32), true},
		{"edge:" + strings.Repeat("a", 33), false},
		{"edge:", false},
		{"inner:", false},
		{"edge:AMS", false},
		{"edge:ams_1", false},
		{"inner:core 1", false},
		{"hop:ams-1", false},
		{"Direct", false},
		{"tunnel", false},
		{"", false},
	} {
		if got := validMonPath(tc.path); got != tc.ok {
			t.Errorf("validMonPath(%q) = %v, want %v", tc.path, got, tc.ok)
		}
	}
}

// TestApplyEventsFeedAndState: duplicates by id, unknown inbounds ignored,
// a late event stays in the feed without rolling the state back, mon_client
// and panel events touch no target, and only unnotified target/mon_client
// events reach the hook, which marks them notified.
func TestApplyEventsFeedAndState(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	rec := &recordingNotifier{}
	SetMonEventNotifier(rec)
	t.Cleanup(func() { SetMonEventNotifier(nil) })
	db := database.GetDB()

	res, err := m.ApplyEvents([]MonEventIn{
		targetEvent(ev1, 2000, "UP", "DOWN", "tls_timeout"),
		{Id: ev2, Ts: 2005, Kind: "mon_client", MonClientId: "msk-1", From: "ONLINE", To: "OFFLINE", Reason: "heartbeat_missed"},
		{Id: ev3, Ts: 1000, Kind: "panel", From: "PANEL_UP", To: "PANEL_DOWN", Reason: "http_timeout"},
		{Id: ev4, Ts: 2001, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", InboundId: 99, Path: "proxy", From: "UP", To: "DOWN", Reason: "tcp_refused"},
	})
	if err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	if res.Accepted != 3 || res.Duplicates != 0 || len(res.Ignored) != 1 || res.Ignored[0].Id != ev4 || res.Ignored[0].Error != "unknown_inbound" {
		t.Errorf("first batch: %+v", res)
	}
	var target model.MonTarget
	if err := db.Where("mon_client_id = ? AND inbound_id = ?", "ams-1", 1).First(&target).Error; err != nil {
		t.Fatalf("target row: %v", err)
	}
	if target.State != "DOWN" || target.Since != 2000 || target.Reason != "tls_timeout" || target.Path != "proxy" {
		t.Errorf("target after DOWN: %+v", target)
	}
	var targets int64
	db.Model(&model.MonTarget{}).Count(&targets)
	if targets != 1 {
		t.Errorf("%d target rows, want 1 (mon_client/panel events create none)", targets)
	}
	var panel model.MonEvent
	db.First(&panel, "id = ?", ev3)
	if !panel.Notified || panel.ReceivedAt == 0 {
		t.Errorf("panel event stored as %+v, want notified=true and received_at set", panel)
	}
	if len(rec.got) != 2 || rec.got[0].Id != ev1 || rec.got[1].Id != ev2 {
		t.Errorf("notifier got %v, want the target and mon_client events in ts order", rec.got)
	}
	var notified int64
	db.Model(&model.MonEvent{}).Where("notified = ?", true).Count(&notified)
	if notified != 3 {
		t.Errorf("%d events notified after the hook, want 3", notified)
	}

	// Same id again: a duplicate, nothing changes, no notification.
	rec.got = nil
	res, err = m.ApplyEvents([]MonEventIn{targetEvent(ev1, 2000, "UP", "DOWN", "tls_timeout")})
	if err != nil || res.Accepted != 0 || res.Duplicates != 1 || len(rec.got) != 0 {
		t.Errorf("duplicate: res=%+v err=%v notified=%v", res, err, rec.got)
	}

	// A late UP (ts before the current since) is kept in the feed but does not
	// roll the state back; a newer UP does.
	res, err = m.ApplyEvents([]MonEventIn{
		targetEvent(ev5, 1500, "DOWN", "UP", "recovered"),
	})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("late event: res=%+v err=%v", res, err)
	}
	db.Where("mon_client_id = ? AND inbound_id = ?", "ams-1", 1).First(&target)
	if target.State != "DOWN" || target.Since != 2000 {
		t.Errorf("a late event rolled the target back: %+v", target)
	}
	var feed int64
	db.Model(&model.MonEvent{}).Where("id = ?", ev5).Count(&feed)
	if feed != 1 {
		t.Error("the late event is missing from the feed")
	}
	rec.got = nil
	if _, err := m.ApplyEvents([]MonEventIn{{Id: "019254a0-ce55-7274-8061-6e7f8091a2b3", Ts: 3000, Kind: "target", MonClientId: "ams-1",
		InboundKind: "xray", InboundId: 1, Path: "proxy", From: "DOWN", To: "UP", Reason: "recovered", Notified: true}}); err != nil {
		t.Fatal(err)
	}
	db.Where("mon_client_id = ? AND inbound_id = ?", "ams-1", 1).First(&target)
	if target.State != "UP" || target.Since != 3000 || target.Reason != "recovered" {
		t.Errorf("a newer UP did not move the target: %+v", target)
	}
	if len(rec.got) != 0 {
		t.Errorf("a notified=true event reached the hook: %v", rec.got)
	}
}

func stat(client string, id int, path string, bucket int64, ok, fail int, min, avg, max *int64) MonStatIn {
	return MonStatIn{MonClientId: client, InboundKind: "xray", InboundId: id, Path: path, BucketStart: bucket,
		NOk: ok, NFail: fail, LatencyMinMs: min, LatencyAvgMs: avg, LatencyMaxMs: max}
}

// TestUpsertStatsAndRollup: a repeated key replaces the row; the rollup of the
// hour is recomputed with n_buckets showing how many 5-minute rows went in,
// lat_avg weighted by n_ok, extremes for min/max/handshake, and a failed
// bucket contributing no latency; the target row appears from the first
// aggregate; old buckets and unknown inbounds are ignored.
func TestUpsertStatsAndRollup(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	db := database.GetDB()
	hour := (time.Now().UnixMilli() / 3600000) * 3600000 // the current hour, well inside retention
	old := hour - 8*24*3600000                           // eight days back: past the 7-day default

	res, err := m.UpsertStats([]MonStatIn{
		stat("ams-1", 1, "proxy", hour, 5, 0, i64(30), i64(40), i64(50)),
		stat("ams-1", 1, "proxy", hour+monBucketMs, 15, 1, i64(20), i64(80), i64(90)),
		stat("ams-1", 1, "proxy", hour+2*monBucketMs, 0, 5, i64(1), i64(1), i64(1)), // failed bucket: latency must be nulled
		stat("ams-1", 1, "proxy", old, 5, 0, i64(1), i64(1), i64(1)),
		stat("ams-1", 99, "proxy", hour, 5, 0, i64(1), i64(1), i64(1)),
	})
	if err != nil {
		t.Fatalf("UpsertStats: %v", err)
	}
	if res.Accepted != 3 || len(res.Ignored) != 2 {
		t.Fatalf("result: %+v", res)
	}
	reasons := map[string]bool{}
	for _, ig := range res.Ignored {
		reasons[ig.Error] = true
	}
	if !reasons["retention_expired"] || !reasons["unknown_inbound"] {
		t.Errorf("ignored reasons: %+v", res.Ignored)
	}

	var failed model.MonStatsCurrent
	db.Where("bucket_start = ?", hour+2*monBucketMs).First(&failed)
	if failed.LatMin != nil || failed.LatAvg != nil || failed.LatMax != nil || failed.NFail != 5 || failed.BucketMs != monBucketMs {
		t.Errorf("failed bucket stored as %+v, want NULL latency", failed)
	}
	var rollup model.MonStatsRollup
	if err := db.Where("step_ms = ? AND bucket_start = ?", 3600000, hour).First(&rollup).Error; err != nil {
		t.Fatalf("rollup row: %v", err)
	}
	if rollup.NBuckets != 3 || rollup.NOk != 20 || rollup.NFail != 6 ||
		rollup.LatMin == nil || *rollup.LatMin != 20 || rollup.LatMax == nil || *rollup.LatMax != 90 ||
		rollup.LatAvg == nil || *rollup.LatAvg != (5*40+15*80)/20 || rollup.HandshakeMs != nil {
		t.Errorf("rollup: %+v (min %v avg %v max %v)", rollup, rollup.LatMin, rollup.LatAvg, rollup.LatMax)
	}
	var target model.MonTarget
	if err := db.Where("mon_client_id = ? AND inbound_id = ? AND path = ?", "ams-1", 1, "proxy").First(&target).Error; err != nil {
		t.Fatalf("target from first aggregate: %v", err)
	}
	if target.State != "UNKNOWN" || target.Since != hour {
		t.Errorf("target from aggregate: %+v", target)
	}

	// The same key again replaces the row and the rollup follows.
	if _, err := m.UpsertStats([]MonStatIn{stat("ams-1", 1, "proxy", hour, 10, 0, i64(10), i64(20), i64(30))}); err != nil {
		t.Fatal(err)
	}
	var current int64
	db.Model(&model.MonStatsCurrent{}).Count(&current)
	if current != 3 {
		t.Errorf("%d current rows after a repeated key, want 3", current)
	}
	db.Where("step_ms = ? AND bucket_start = ?", 3600000, hour).First(&rollup)
	if rollup.NBuckets != 3 || rollup.NOk != 25 || *rollup.LatMin != 10 || *rollup.LatAvg != (10*20+15*80)/25 {
		t.Errorf("rollup after replace: %+v (min %v avg %v)", rollup, rollup.LatMin, rollup.LatAvg)
	}

	// The handshake column is the maximum across the hour.
	awgIn := stat("ams-1", 1, "direct", hour, 1, 0, i64(1), i64(1), i64(1))
	awgIn.HandshakeMs = i64(120)
	awgIn2 := stat("ams-1", 1, "direct", hour+monBucketMs, 1, 0, i64(1), i64(1), i64(1))
	awgIn2.HandshakeMs = i64(45)
	if _, err := m.UpsertStats([]MonStatIn{awgIn, awgIn2}); err != nil {
		t.Fatal(err)
	}
	var direct model.MonStatsRollup
	db.Where("step_ms = ? AND bucket_start = ? AND path = ?", 3600000, hour, "direct").First(&direct)
	if direct.HandshakeMs == nil || *direct.HandshakeMs != 120 || direct.NBuckets != 2 {
		t.Errorf("handshake rollup: %+v (%v)", direct, direct.HandshakeMs)
	}

	// Bad elements are rejected by index; the good one beside them lands.
	res, err = m.UpsertStats([]MonStatIn{
		stat("ams-1", 1, "proxy", hour+1, 1, 0, nil, nil, nil),
		stat("ams-1", 1, "inner:core-1", hour, 3, 0, i64(5), i64(6), i64(7)),
		stat("ams-1", 1, "tunnel", hour, 1, 0, nil, nil, nil),
	})
	if err != nil {
		t.Fatalf("mixed stats batch: %v", err)
	}
	if res.Accepted != 1 || len(res.Rejected) != 2 || res.Rejected[0].Index != 0 || res.Rejected[1].Index != 2 ||
		!strings.Contains(res.Rejected[0].Error, "stats[0].bucketStart") || !strings.Contains(res.Rejected[1].Error, "stats[2].path") {
		t.Errorf("mixed stats batch: %+v", res)
	}
	var inner int64
	db.Model(&model.MonStatsCurrent{}).Where("path = ?", "inner:core-1").Count(&inner)
	if inner != 1 {
		t.Errorf("%d inner:core-1 rows, want 1", inner)
	}
}

// TestWorstLiveTargetState: the badge folds only targets of mon-clients in
// the snapshot, by DOWN > FLAPPING > UNKNOWN > UP > PAUSED.
func TestWorstLiveTargetState(t *testing.T) {
	m := newMonitoringTestService(t)
	db := database.GetDB()
	for _, row := range []model.MonTarget{
		{MonClientId: "gone-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "DOWN"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "UP"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct", State: "PAUSED"},
		{MonClientId: "msk-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "FLAPPING"},
		{MonClientId: "msk-1", InboundKind: "xray", InboundId: 2, Path: "proxy", State: "UP"},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := m.WorstLiveTargetState("xray", 1); got != "" {
		t.Errorf("with an empty registry = %q, want none", got)
	}
	if err := m.setRegistrySnapshot([]MonClient{{Id: "ams-1"}, {Id: "msk-1"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   int
		want string
	}{{1, "FLAPPING"}, {2, "UP"}, {3, ""}} {
		if got, err := m.WorstLiveTargetState("xray", tc.id); err != nil || got != tc.want {
			t.Errorf("inbound %d: %q, %v; want %q", tc.id, got, err, tc.want)
		}
	}
	if err := m.setRegistrySnapshot([]MonClient{{Id: "ams-1"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.WorstLiveTargetState("xray", 1); got != "UP" {
		t.Errorf("after msk-1 left = %q, want UP", got)
	}
}
