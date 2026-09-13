package database

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestMonitoringSchemaMigrates: the four monitoring tables and every idx_mon_
// index come up on a fresh database, on the table namedIndexes says they
// belong on.
func TestMonitoringSchemaMigrates(t *testing.T) {
	if err := InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	for _, table := range []string{"mon_targets", "mon_events", "mon_stats_current", "mon_stats_rollup"} {
		if !tableExists(table) {
			t.Errorf("table %s was not created", table)
		}
	}
	type row struct {
		Name    string
		TblName string
	}
	var rows []row
	if err := db.Raw(`SELECT name, tbl_name FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_mon_%'`).
		Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, r := range rows {
		found[r.Name] = r.TblName
	}
	for name, table := range namedIndexes {
		if len(name) < 8 || name[:8] != "idx_mon_" {
			continue
		}
		if got, ok := found[name]; !ok {
			t.Errorf("index %s missing", name)
		} else if got != table {
			t.Errorf("index %s is on %s, namedIndexes says %s", name, got, table)
		}
	}
	if len(found) != 8 {
		t.Errorf("expected 8 idx_mon_ indexes, found %d: %v", len(found), found)
	}
}

// TestDeleteMonitoringForInboundCascadesAllFourTables: deleting one inbound's
// monitoring wipes its rows in every table and touches nothing of the other
// inbound, including the AWG key that shares inboundId 0 with nobody.
func TestDeleteMonitoringForInboundCascadesAllFourTables(t *testing.T) {
	if err := InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seed := func(kind string, id int, suffix string) {
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(db.Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: "direct", State: "UP"}).Error)
		must(db.Create(&model.MonEvent{Id: "0192-" + suffix, Ts: 1, Kind: "target", MonClientId: "ams-1",
			InboundKind: kind, InboundId: id, Path: "direct", FromState: "UP", ToState: "DOWN"}).Error)
		must(db.Create(&model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: "direct",
			BucketStart: 300_000, BucketMs: 300_000, NOk: 5}).Error)
		must(db.Create(&model.MonStatsRollup{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: "direct",
			StepMs: 3_600_000, BucketStart: 0, NBuckets: 1, NOk: 5}).Error)
	}
	seed(model.MonKindXray, 12, "a")
	seed(model.MonKindXray, 13, "b")
	seed(model.MonKindAwg, 0, "c")

	if err := DeleteMonitoringForInbound(db, model.MonKindXray, 12); err != nil {
		t.Fatalf("cascade: %v", err)
	}

	count := func(table any, kind string, id int) int64 {
		var n int64
		db.Model(table).Where("inbound_kind = ? AND inbound_id = ?", kind, id).Count(&n)
		return n
	}
	for _, table := range []any{&model.MonTarget{}, &model.MonEvent{}, &model.MonStatsCurrent{}, &model.MonStatsRollup{}} {
		if n := count(table, model.MonKindXray, 12); n != 0 {
			t.Errorf("%T: %d rows left for the deleted inbound", table, n)
		}
		if n := count(table, model.MonKindXray, 13); n != 1 {
			t.Errorf("%T: other xray inbound lost rows, have %d", table, n)
		}
		if n := count(table, model.MonKindAwg, 0); n != 1 {
			t.Errorf("%T: awg rows touched, have %d", table, n)
		}
	}
}
