package service

import (
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// monSeedPath stores one target, one event and one bucket of each aggregate
// table for a path, so a test can tell which of them a registry write took.
func monSeedPath(t *testing.T, path string) {
	t.Helper()
	db := database.GetDB()
	rows := []any{
		&model.MonTarget{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: path, State: "UP"},
		&model.MonEvent{Id: "ev-" + path, Ts: 1, ReceivedAt: 1, Kind: "target", MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: path, To: "UP"},
		&model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: path, BucketStart: 300000, BucketMs: 300000, NOk: 1},
		&model.MonStatsRollup{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: path, StepMs: 3600000, BucketStart: 0, NBuckets: 1, NOk: 1},
	}
	for _, r := range rows {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
}

// monPathsIn lists the distinct paths left in a monitoring table, sorted.
func monPathsIn(t *testing.T, m any) string {
	t.Helper()
	var paths []string
	if err := database.GetDB().Model(m).Distinct("path").Order("path").Pluck("path", &paths).Error; err != nil {
		t.Fatal(err)
	}
	return strings.Join(paths, ",")
}

// TestProbedSetChangesPruneTargets (proxy-chain.md §6.1, contract §6): every
// registry write that changes the probed set deletes, in its own
// transaction, the mon_targets rows of the paths that left it — proxy when
// the first hop joins, a hop's path when it is renamed, reissued back to
// pending or starts draining, and proxy's return when the last hop goes —
// while events and aggregates stay for the retention.
func TestProbedSetChangesPruneTargets(t *testing.T) {
	s := newChainService(t)
	for _, p := range []string{"direct", "proxy", "edge:edge-a", "edge:ghost"} {
		monSeedPath(t, p)
	}

	// A pending hop changes nothing yet.
	edgeA := addHop(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct,edge:edge-a,edge:ghost,proxy" {
		t.Errorf("targets after adding a pending hop = %s", got)
	}
	// The first join takes proxy away, and a path no hop owns.
	joinHop(t, s, edgeA)
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct,edge:edge-a" {
		t.Errorf("targets after the first join = %s", got)
	}
	if got := monPathsIn(t, &model.MonEvent{}); got != "direct,edge:edge-a,edge:ghost,proxy" {
		t.Errorf("events after the first join = %s, want all of them kept", got)
	}

	// A rename is a new path; the old one's history stays.
	name := "edge-z"
	if err := s.Update(edgeA.Id, UpdateHopInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	monSeedPath(t, "edge:edge-z")
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct,edge:edge-z" {
		t.Errorf("targets after a rename = %s", got)
	}
	if got := monPathsIn(t, &model.MonStatsCurrent{}); !strings.Contains(got, "edge:edge-a") {
		t.Errorf("stats after a rename = %s, want the old name's history kept", got)
	}

	// Reissuing the token puts the hop back to pending: its path leaves the
	// set, and with no probed hop left proxy is in it again.
	if _, _, err := s.ReissueToken(edgeA.Id); err != nil {
		t.Fatal(err)
	}
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct" {
		t.Errorf("targets with the only hop pending = %s", got)
	}
	if err := database.GetDB().Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "proxy", State: "UP"}).Error; err != nil {
		t.Fatal(err)
	}
	host := "a2.example.net"
	if err := s.Update(edgeA.Id, UpdateHopInput{Host: &host}); err != nil {
		t.Fatal(err)
	}
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct,proxy" {
		t.Errorf("targets after a write that keeps the set = %s", got)
	}
}

// TestDrainingPrunesAndDeletionCascades: a hop that starts draining loses its
// targets at once; when its row finally goes, its history goes with it.
func TestDrainingPrunesAndDeletionCascades(t *testing.T) {
	s := newChainService(t)
	inner := addJoined(t, s, AddHopInput{Name: "core-1", Host: "10.0.0.1", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	for _, p := range []string{"direct", "inner:core-1", "edge:edge-a"} {
		monSeedPath(t, p)
	}

	res, err := s.Delete(inner.Id, false, false)
	if err != nil || res.State != DeleteStateDraining {
		t.Fatalf("delete inner: %+v, %v", res, err)
	}
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct,edge:edge-a" {
		t.Errorf("targets while draining = %s", got)
	}
	if got := monPathsIn(t, &model.MonEvent{}); got != "direct,edge:edge-a,inner:core-1" {
		t.Errorf("events while draining = %s, want the history kept", got)
	}

	expireDrain(t, "core-1")
	if err := s.SweepDraining(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []any{&model.MonEvent{}, &model.MonStatsCurrent{}, &model.MonStatsRollup{}} {
		if got := monPathsIn(t, m); got != "direct,edge:edge-a" {
			t.Errorf("%T after the row went = %s", m, got)
		}
	}
}

// TestDeletingAHopCascades (proxy-chain.md §6.1): deleting a hop deletes
// every monitoring row of its path in the same transaction, and of path
// proxy as well when it was the active edge; other paths stay.
func TestDeletingAHopCascades(t *testing.T) {
	s := newChainService(t)
	edgeA := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	edgeB := addJoined(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	if err := s.SetActive(edgeA.Id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"direct", "proxy", "edge:edge-a", "edge:edge-b"} {
		monSeedPath(t, p)
	}

	if _, err := s.Delete(edgeB.Id, false, false); err != nil {
		t.Fatal(err)
	}
	for _, m := range []any{&model.MonEvent{}, &model.MonStatsCurrent{}, &model.MonStatsRollup{}} {
		if got := monPathsIn(t, m); got != "direct,edge:edge-a,proxy" {
			t.Errorf("%T after deleting the standby = %s", m, got)
		}
	}

	// The last, active edge goes by force: its path and proxy's history go,
	// and with no probed hop left proxy is back in the set.
	if _, err := s.Delete(edgeA.Id, true, false); err != nil {
		t.Fatal(err)
	}
	for _, m := range []any{&model.MonEvent{}, &model.MonStatsCurrent{}, &model.MonStatsRollup{}, &model.MonTarget{}} {
		if got := monPathsIn(t, m); got != "direct" {
			t.Errorf("%T after deleting the active edge = %s", m, got)
		}
	}
}

// TestLegacyMigrationPrunesProxy: importing the legacy override makes the
// legacy hop the first probed one, so path proxy leaves the set.
func TestLegacyMigrationPrunesProxy(t *testing.T) {
	s := newChainService(t)
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	monSeedPath(t, "direct")
	monSeedPath(t, "proxy")
	if err := s.MigrateLegacyOverride(); err != nil {
		t.Fatal(err)
	}
	if got := monPathsIn(t, &model.MonTarget{}); got != "direct" {
		t.Errorf("targets after the migration = %s", got)
	}
}
