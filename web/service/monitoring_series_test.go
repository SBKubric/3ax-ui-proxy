package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestSeriesNullGapsAndStepSelection: a range up to a day comes from the
// 5-minute rows with stepMs 300000 and one point per bucket, null where no
// bucket arrived; a longer range comes from the rollup with the configured
// step; series are keyed by mon-client × path.
func TestSeriesNullGapsAndStepSelection(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	now := time.Now().Truncate(5 * time.Minute)
	from := now.Add(-time.Hour).UnixMilli()
	if _, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{
		stat("ams-1", ib.Id, from, 5, 0, i64(40)),
		stat("ams-1", ib.Id, from+2*300_000, 4, 1, i64(50)),  // bucket 1 missing
		stat("ams-1", ib.Id, from+11*300_000, 5, 0, i64(45)), // last bucket of the hour
		{MonClientId: "msk-1", InboundKind: "xray", InboundId: ib.Id, Path: "proxy", BucketStart: from + 300_000, NOk: 0, NFail: 5},
	}}); err != nil {
		t.Fatal(err)
	}

	res, err := m.Series("xray", ib.Id, "1h", now)
	if err != nil {
		t.Fatalf("Series 1h: %v", err)
	}
	if res.StepMs != 300_000 || res.From != from || res.To != now.UnixMilli() || len(res.Series) != 2 { // now sits on the grid
		t.Fatalf("1h response: %+v", res)
	}
	ams := res.Series[0]
	if ams.MonClientId != "ams-1" || ams.Path != "direct" || len(ams.Points) != 12 {
		t.Fatalf("ams series: %+v", ams)
	}
	if ams.Points[0] == nil || ams.Points[0].T != from || ams.Points[0].NOk != 5 || *ams.Points[0].LatAvg != 40 {
		t.Errorf("point 0: %+v", ams.Points[0])
	}
	if ams.Points[1] != nil {
		t.Errorf("missing bucket must be null, got %+v", ams.Points[1])
	}
	if ams.Points[2] == nil || ams.Points[2].NFail != 1 || ams.Points[11] == nil || ams.Points[11].NOk != 5 {
		t.Errorf("points 2 and 11: %+v %+v", ams.Points[2], ams.Points[11])
	}
	for i := 3; i < 11; i++ {
		if ams.Points[i] != nil {
			t.Errorf("point %d should be null", i)
		}
	}
	msk := res.Series[1]
	if msk.MonClientId != "msk-1" || msk.Path != "proxy" || msk.Points[1] == nil || msk.Points[1].NFail != 5 || msk.Points[1].LatAvg != nil {
		t.Errorf("msk series: %+v", msk)
	}

	// 24h: still the fine rows, 288 points.
	day, err := m.Series("xray", ib.Id, "24h", now)
	if err != nil || day.StepMs != 300_000 || len(day.Series[0].Points) != 288 {
		t.Fatalf("24h: %v %+v", err, day)
	}

	// 7d: the rollup at the configured hour step, 168 points, the hour with
	// data holding the summed counts.
	week, err := m.Series("xray", ib.Id, "7d", now)
	if err != nil {
		t.Fatalf("Series 7d: %v", err)
	}
	if week.StepMs != 3_600_000 || len(week.Series) != 2 || len(week.Series[0].Points) != 168 {
		t.Fatalf("7d response: step %d, %d series, %d points", week.StepMs, len(week.Series), len(week.Series[0].Points))
	}
	var filled, nOk, nFail int
	for _, p := range week.Series[0].Points {
		if p != nil {
			filled++
			nOk += p.NOk
			nFail += p.NFail
		}
	}
	if filled < 1 || filled > 2 || nOk != 14 || nFail != 1 {
		t.Errorf("an hour of data should fill one or two hourly points summing to 14/1, got %d points %d/%d", filled, nOk, nFail)
	}
	month, err := m.Series("xray", ib.Id, "30d", now)
	if err != nil || len(month.Series[0].Points) != 720 {
		t.Fatalf("30d: %v", err)
	}
	if _, err := m.Series("xray", ib.Id, "2h", now); err == nil {
		t.Error("unknown range accepted")
	}
	if _, err := m.Series("wg", ib.Id, "1h", now); err == nil {
		t.Error("unknown kind accepted")
	}
}

// TestTargetsAndEvents: GET targets folds live target rows with their last
// day and the inbound Health; GET events pages by ts, clamps the limit and
// marks events of retired mon-clients.
func TestTargetsAndEvents(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	ib := seedXrayInbound(t, &InboundService{}, "reality", true)
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", Name: "Amsterdam #1", State: "ONLINE"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(5 * time.Minute)
	if _, err := m.UpsertStats(&MonStatsBatch{Stats: []MonStatIn{
		stat("ams-1", ib.Id, now.Add(-time.Hour).UnixMilli(), 9, 1, i64(40)),
		stat("ams-1", ib.Id, now.Add(-2*time.Hour).UnixMilli(), 10, 0, i64(60)),
		stat("old-1", ib.Id, now.Add(-time.Hour).UnixMilli(), 0, 10, nil),
	}}); err != nil {
		t.Fatal(err)
	}
	var events []MonEventIn
	for i := 0; i < 205; i++ {
		e := targetEvent(t, now.Add(-time.Duration(i)*time.Minute).UnixMilli(), "ams-1", ib.Id, "UP", "DOWN", "tls_timeout")
		e.Path = model.MonPathDirect
		e.Notified = true
		events = append(events, e)
	}
	retired := targetEvent(t, now.Add(time.Second).UnixMilli(), "old-1", ib.Id, "UP", "DOWN", "gone")
	retired.Notified = true
	if _, err := m.ApplyEvents(&MonEventsBatch{Events: append(events, retired)}); err != nil {
		t.Fatal(err)
	}
	// old-1 is not in the snapshot: EnsureProbeSet pruned it once, but the
	// stats above recreated its row; refresh the snapshot to prune again.
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", Name: "Amsterdam #1", State: "ONLINE"}}); err != nil {
		t.Fatal(err)
	}

	res, err := m.Targets(now)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].MonClientId != "ams-1" || res.Targets[0].State != "DOWN" {
		t.Fatalf("live targets: %+v", res.Targets)
	}
	tv := res.Targets[0]
	if tv.Uptime24h == nil || *tv.Uptime24h != 0.95 || tv.LatAvg24h == nil || *tv.LatAvg24h != 50 || tv.Cov24h < 0.0069 || tv.Cov24h > 0.007 {
		t.Errorf("last day of the target: uptime %v lat %v cov %v", tv.Uptime24h, tv.LatAvg24h, tv.Cov24h)
	}
	if len(res.Inbounds) != 1 || res.Inbounds[0].Health != "DOWN" || len(res.MonClients) != 1 || res.Stale || res.Probe.Count != 1 {
		t.Errorf("inbounds/snapshot/probe: %+v", res)
	}

	page, err := m.Events(0, 500, "", 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(page) != MonEventsMax {
		t.Fatalf("limit not clamped: %d", len(page))
	}
	if page[0].MonClientId != "old-1" || !page[0].Retired || page[0].InboundRemark != "reality" {
		t.Errorf("newest event should be the retired mon-client's: %+v", page[0])
	}
	if page[1].Retired || page[1].Ts > page[0].Ts {
		t.Errorf("ordering/retired flag: %+v", page[1])
	}
	next, err := m.Events(page[len(page)-1].Ts, 50, "xray", ib.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 6 || next[0].Ts >= page[len(page)-1].Ts {
		t.Errorf("paging before the last ts: %d events", len(next))
	}
	other, _ := m.Events(0, 10, "xray", ib.Id+1)
	if len(other) != 0 {
		t.Errorf("filter by inbound leaked %d events", len(other))
	}
	_ = database.GetDB
}
