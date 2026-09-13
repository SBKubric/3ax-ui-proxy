package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestSummaryUptimeCoverageIncidents: uptime is Σ n_ok / Σ (n_ok + n_fail)
// over every mon-client, coverage the share of expected buckets that came,
// incidents the DOWN events of the window; a path with no data has a null
// uptime; the worst target needs at least half coverage and a live
// mon-client; OFFLINE mon-clients come from the snapshot.
func TestSummaryUptimeCoverageIncidents(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	ib := seedXrayInbound(t, is, "reality", true)
	quiet := seedXrayInbound(t, is, "quiet", true)
	if _, err := m.EnsureProbeSet([]MonClient{
		{Id: "ams-1", Name: "Amsterdam #1", Region: "NL", State: "ONLINE"},
		{Id: "msk-1", Name: "Moscow #1", Region: "RU", State: "OFFLINE"},
	}); err != nil {
		t.Fatal(err)
	}
	// Aligned to the bucket grid so the hour holds exactly twelve buckets.
	now := time.Now().Truncate(5 * time.Minute)
	window := time.Hour
	start := now.Add(-window).UnixMilli()

	var stats []MonStatIn
	for i := 0; i < 12; i++ { // ams-1 direct: full hour, 60 ok
		stats = append(stats, MonStatIn{MonClientId: "ams-1", InboundKind: "xray", InboundId: ib.Id, Path: "direct",
			BucketStart: start + int64(i)*300_000, NOk: 5, NFail: 0})
	}
	for i := 0; i < 6; i++ { // msk-1 direct: half the hour, 20 ok 10 fail
		stats = append(stats, MonStatIn{MonClientId: "msk-1", InboundKind: "xray", InboundId: ib.Id, Path: "direct",
			BucketStart: start + int64(i)*300_000, NOk: 4, NFail: 1})
	}
	for i := 0; i < 3; i++ { // ams-1 proxy: a quarter of the hour, all failing — coverage too low to be "worst"
		stats = append(stats, MonStatIn{MonClientId: "ams-1", InboundKind: "xray", InboundId: ib.Id, Path: "proxy",
			BucketStart: start + int64(i)*300_000, NOk: 0, NFail: 5})
	}
	// A mon-client that left the registry: never the worst, but its probes count for the inbound.
	stats = append(stats, MonStatIn{MonClientId: "old-1", InboundKind: "xray", InboundId: ib.Id, Path: "direct",
		BucketStart: start, NOk: 0, NFail: 0})
	if _, err := m.UpsertStats(&MonStatsBatch{Stats: stats}); err != nil {
		t.Fatal(err)
	}
	events := []MonEventIn{
		targetEvent(t, start+300_000, "ams-1", ib.Id, "UP", "DOWN", "tls_timeout"),
		targetEvent(t, start+600_000, "ams-1", ib.Id, "DOWN", "UP", "recovered"),
		targetEvent(t, start+900_000, "msk-1", ib.Id, "UP", "DOWN", "tcp_timeout"),
		targetEvent(t, now.Add(-2*window).UnixMilli(), "msk-1", ib.Id, "UP", "DOWN", "outside the window"),
	}
	for i := range events {
		events[i].Notified = true
		events[i].Path = model.MonPathDirect
	}
	if _, err := m.ApplyEvents(&MonEventsBatch{Events: events}); err != nil {
		t.Fatal(err)
	}

	summary, err := m.Summary(now, window)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if len(summary.Inbounds) != 2 {
		t.Fatalf("inbounds: %+v", summary.Inbounds)
	}
	var reality, quietEntry *MonSummaryInbound
	for i := range summary.Inbounds {
		switch summary.Inbounds[i].InboundId {
		case ib.Id:
			reality = &summary.Inbounds[i]
		case quiet.Id:
			quietEntry = &summary.Inbounds[i]
		}
	}
	if reality == nil || quietEntry == nil {
		t.Fatalf("inbounds: %+v", summary.Inbounds)
	}
	if len(quietEntry.Paths) != 0 || quietEntry.Incidents != 0 {
		t.Errorf("an inbound without data: %+v", quietEntry)
	}
	if len(reality.Paths) != 2 || reality.Incidents != 2 {
		t.Fatalf("reality: %+v", reality)
	}
	direct, proxy := reality.Paths[0], reality.Paths[1]
	if direct.Path != "direct" || direct.NOk != 84 || direct.NFail != 6 || direct.Uptime == nil || *direct.Uptime < 0.933 || *direct.Uptime > 0.934 {
		t.Errorf("direct uptime should be 84/90: %+v", direct)
	}
	// 12 + 6 + 1 buckets over three series × 12 expected.
	if direct.Coverage < 0.527 || direct.Coverage > 0.528 || direct.Incidents != 2 {
		t.Errorf("direct coverage/incidents: %+v", direct)
	}
	if proxy.Path != "proxy" || proxy.Uptime == nil || *proxy.Uptime != 0 || proxy.Coverage != 0.25 || proxy.Incidents != 0 {
		t.Errorf("proxy: %+v", proxy)
	}
	if summary.Worst == nil || summary.Worst.MonClientId != "msk-1" || summary.Worst.Path != "direct" || summary.Worst.Uptime != 0.8 || summary.Worst.Coverage != 0.5 {
		t.Errorf("worst should be msk-1 direct at 80%% with half coverage: %+v", summary.Worst)
	}
	if len(summary.OfflineMonClients) != 1 || summary.OfflineMonClients[0].Id != "msk-1" {
		t.Errorf("offline mon-clients: %+v", summary.OfflineMonClients)
	}
	if summary.StaleMs != 0 || summary.From != now.Add(-window).UnixMilli() || summary.To != now.UnixMilli() {
		t.Errorf("window/stale: %+v", summary)
	}
}

// TestSummaryStaleTimeAndEmpty: STALE time sums the closed spans and the open
// one inside the window, and a panel with no data still answers with every
// inbound and null uptimes.
func TestSummaryStaleTimeAndEmpty(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	seedXrayInbound(t, &InboundService{}, "reality", true)
	now := time.Now()
	summary, err := m.Summary(now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Inbounds) != 1 || len(summary.Inbounds[0].Paths) != 0 || summary.Worst != nil || summary.StaleMs != 0 {
		t.Errorf("empty summary: %+v", summary)
	}

	// Silent from -60m, noticed at -40m, back at -30m: a closed 30-minute span
	// (since counts from the last contact); then silent again from -20m and
	// still silent: an open 20-minute span → 50 minutes.
	m.TouchLastContact(now.Add(-60 * time.Minute))
	if !m.CheckStale(now.Add(-40 * time.Minute)) {
		t.Fatal("stale not raised")
	}
	m.RecordContact(now.Add(-30 * time.Minute))
	m.TouchLastContact(now.Add(-20 * time.Minute))
	if !m.CheckStale(now) {
		t.Fatal("stale not raised again")
	}
	summary, err = m.Summary(now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := summary.StaleMs / 60_000; got != 50 {
		t.Errorf("stale minutes = %d, want 50", got)
	}
	// A window that starts inside the closed span only counts the overlap.
	summary, _ = m.Summary(now, 45*time.Minute)
	if got := summary.StaleMs / 60_000; got != 35 {
		t.Errorf("stale minutes in a 45-minute window = %d, want 35", got)
	}

	if got := monDuration(45_000); got != "45s" {
		t.Errorf("monDuration(45s) = %q", got)
	}
	if got := monDuration(7 * 60_000); got != "7m" {
		t.Errorf("monDuration(7m) = %q", got)
	}
	if got := monDuration((2*60 + 5) * 60_000); got != "2h 5m" {
		t.Errorf("monDuration(2h5m) = %q", got)
	}
	if got := monDuration((3*24 + 4) * 3_600_000); got != "3d 4h" {
		t.Errorf("monDuration(3d4h) = %q", got)
	}
	if got := monPercent(0.9985); got != "99.9%" {
		t.Errorf("monPercent = %q", got)
	}
	if got := monClientLabel(MonClient{Id: "ams-1"}); got != "ams-1" {
		t.Errorf("monClientLabel fallback = %q", got)
	}
	_ = model.MonPathDirect
}
