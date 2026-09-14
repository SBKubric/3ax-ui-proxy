package database

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"gorm.io/gorm"
)

func initMonitoringTestDB(t *testing.T) {
	t.Helper()
	if err := InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB() })
}

// TestMonitoringSchema guards the four monitoring tables of spec §2.1: their
// column names (the wire form and the service layer address them by name),
// and every idx_mon_* index sitting on the table namedIndexes says it belongs
// to — SQLite index names are global, so a stray one would block the real one.
func TestMonitoringSchema(t *testing.T) {
	initMonitoringTestDB(t)

	columns := map[string][]string{
		"mon_targets": {"id", "mon_client_id", "inbound_kind", "inbound_id", "path",
			"state", "since", "reason", "updated_at"},
		"mon_events": {"id", "ts", "received_at", "kind", "mon_client_id", "inbound_kind",
			"inbound_id", "path", "from_state", "to_state", "reason", "notified"},
		"mon_stats_current": {"id", "mon_client_id", "inbound_kind", "inbound_id", "path",
			"bucket_start", "bucket_ms", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"},
		"mon_stats_rollup": {"id", "mon_client_id", "inbound_kind", "inbound_id", "path",
			"step_ms", "bucket_start", "n_buckets", "n_ok", "n_fail", "lat_min", "lat_avg", "lat_max", "handshake_ms"},
	}
	for table, want := range columns {
		if !tableExists(table) {
			t.Errorf("table %s was not created", table)
			continue
		}
		for _, col := range want {
			if !columnExists(table, col) {
				t.Errorf("table %s has no column %s", table, col)
			}
		}
	}

	indexes := map[string]string{}
	for name, table := range namedIndexes {
		if len(name) > 8 && name[:8] == "idx_mon_" {
			indexes[name] = table
		}
	}
	if len(indexes) != 8 {
		t.Fatalf("namedIndexes lists %d idx_mon_* indexes, want 8", len(indexes))
	}
	for index, table := range indexes {
		var n int64
		if err := db.Raw(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ? AND tbl_name = ?",
			index, table,
		).Scan(&n).Error; err != nil {
			t.Fatalf("query index %s: %v", index, err)
		}
		if n != 1 {
			t.Errorf("index %s on %s: found %d, want 1", index, table, n)
		}
	}

	// The natural keys must be unique: a repeated POST /stats or a second
	// sighting of a target replaces, never duplicates.
	for _, tc := range []struct {
		index string
		row   any
	}{
		{"idx_mon_targets_key", &model.MonTarget{MonClientId: "ams-1", InboundKind: model.MonInboundKindXray, InboundId: 12, Path: model.MonPathProxy, State: model.MonStateUp}},
		{"idx_mon_stats_current_key", &model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: model.MonInboundKindXray, InboundId: 12, Path: model.MonPathProxy, BucketStart: 300000}},
		{"idx_mon_stats_rollup_key", &model.MonStatsRollup{MonClientId: "ams-1", InboundKind: model.MonInboundKindXray, InboundId: 12, Path: model.MonPathProxy, StepMs: 3600000, BucketStart: 0}},
	} {
		if err := db.Create(tc.row).Error; err != nil {
			t.Fatalf("%s: first insert: %v", tc.index, err)
		}
		if err := db.Create(tc.row).Error; err == nil {
			t.Errorf("%s: a second row with the same key was accepted", tc.index)
		}
	}
	if err := db.Create(&model.MonEvent{Id: "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", Ts: 1, ReceivedAt: 1, Kind: model.MonEventKindPanel}).Error; err != nil {
		t.Fatalf("event insert: %v", err)
	}
	if err := db.Create(&model.MonEvent{Id: "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", Ts: 2, ReceivedAt: 2, Kind: model.MonEventKindPanel}).Error; err == nil {
		t.Error("mon_events: a second event with the same id was accepted")
	}
}

// TestDeleteMonitoringByInbound checks the cascade of spec §3.6: deleting an
// inbound clears its rows from all four tables and leaves every other
// inbound's rows — and the events that carry no inbound at all — in place.
func TestDeleteMonitoringByInbound(t *testing.T) {
	initMonitoringTestDB(t)

	type key struct {
		kind string
		id   int
	}
	keys := []key{
		{model.MonInboundKindXray, 12},
		{model.MonInboundKindXray, 13},
		{model.MonInboundKindAwg, 0},
	}
	lat := int64(47)
	for i, k := range keys {
		for _, path := range []string{model.MonPathDirect, model.MonPathProxy} {
			if err := db.Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: k.kind, InboundId: k.id, Path: path, State: model.MonStateUp}).Error; err != nil {
				t.Fatalf("target: %v", err)
			}
			if err := db.Create(&model.MonEvent{
				Id: uuidLike(i, path), Ts: 1000, ReceivedAt: 1001, Kind: model.MonEventKindTarget,
				MonClientId: "ams-1", InboundKind: k.kind, InboundId: k.id, Path: path,
				From: model.MonStateUp, To: model.MonStateDown, Reason: "tcp_timeout",
			}).Error; err != nil {
				t.Fatalf("event: %v", err)
			}
			if err := db.Create(&model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: k.kind, InboundId: k.id, Path: path, BucketStart: 300000, BucketMs: 300000, NOk: 5, LatMin: &lat, LatAvg: &lat, LatMax: &lat}).Error; err != nil {
				t.Fatalf("stats current: %v", err)
			}
			if err := db.Create(&model.MonStatsRollup{MonClientId: "ams-1", InboundKind: k.kind, InboundId: k.id, Path: path, StepMs: 3600000, BucketStart: 0, NBuckets: 12, NOk: 60}).Error; err != nil {
				t.Fatalf("stats rollup: %v", err)
			}
		}
	}
	// Events without an inbound: a mon-client going offline, the panel itself.
	for _, e := range []model.MonEvent{
		{Id: "019254a0-8a11-7e30-8c2d-2a3b4c5d6e7f", Ts: 1000, ReceivedAt: 1001, Kind: model.MonEventKindMonClient, MonClientId: "msk-1", From: "ONLINE", To: "OFFLINE", Reason: "heartbeat_missed"},
		{Id: "019254a0-9b22-7f41-9d3e-3b4c5d6e7f80", Ts: 1000, ReceivedAt: 1001, Kind: model.MonEventKindPanel, From: "PANEL_UP", To: "PANEL_DOWN", Reason: "http_timeout", Notified: true},
	} {
		if err := db.Create(&e).Error; err != nil {
			t.Fatalf("inbound-less event: %v", err)
		}
	}

	count := func(table, kind string, id int) int64 {
		var n int64
		if err := db.Table(table).Where("inbound_kind = ? AND inbound_id = ?", kind, id).Count(&n).Error; err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	tables := []string{"mon_targets", "mon_events", "mon_stats_current", "mon_stats_rollup"}

	if err := db.Transaction(func(tx *gorm.DB) error {
		return model.DeleteMonitoringByInbound(tx, model.MonInboundKindXray, 12)
	}); err != nil {
		t.Fatalf("cascade: %v", err)
	}

	for _, table := range tables {
		if n := count(table, model.MonInboundKindXray, 12); n != 0 {
			t.Errorf("%s: %d rows of the deleted inbound remain", table, n)
		}
		for _, k := range keys[1:] {
			if n := count(table, k.kind, k.id); n != 2 {
				t.Errorf("%s: inbound (%s,%d) has %d rows, want 2", table, k.kind, k.id, n)
			}
		}
	}
	var total int64
	db.Model(&model.MonEvent{}).Count(&total)
	if total != 2*2+2 {
		t.Errorf("mon_events: %d rows remain, want 6 (two inbounds × two paths + two inbound-less)", total)
	}
}

// uuidLike builds a distinct 36-character id per (inbound, path) for the test;
// the real ids are UUID v7 strings minted by mon-server.
func uuidLike(i int, path string) string {
	base := "019254a0-0000-7000-8000-00000000000"
	return base[:len(base)-1-len(path)] + path + string(rune('0'+i))
}
