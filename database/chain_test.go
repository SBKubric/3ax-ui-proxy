package database

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// The chain registry is worth nothing if two hops can share a name: the bot,
// the UI and the monitoring path all address a hop by name (§2.1), and the
// document builder looks a hop up by it. The uniqueness lives in the schema,
// as index idx_chain_hops_name, so this checks the migration actually created
// it rather than trusting the struct tag.
func TestChainHopsTableEnforcesUniqueName(t *testing.T) {
	if err := InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB() })

	db := GetDB()
	if !db.Migrator().HasTable(&model.ChainHop{}) {
		t.Fatal("chain_hops was not created by initModels")
	}
	for _, index := range []string{"idx_chain_hops_name", "idx_chain_hops_next", "idx_chain_hops_role"} {
		if !db.Migrator().HasIndex(&model.ChainHop{}, index) {
			t.Errorf("index %s missing", index)
		}
		if table, ok := namedIndexes[index]; !ok || table != "chain_hops" {
			t.Errorf("namedIndexes[%s] = %q, want chain_hops", index, table)
		}
	}

	first := &model.ChainHop{Name: "edge-a", Host: "a.example.net", Role: model.ChainRoleEdge,
		State: model.ChainStatePending, SubPort: 2096, SubScheme: "https"}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("create first hop: %v", err)
	}
	if first.Id == 0 {
		t.Fatal("id was not assigned")
	}
	if first.CreatedAt == 0 || first.UpdatedAt == 0 {
		t.Fatalf("autoCreateTime/autoUpdateTime did not fill: %+v", first)
	}

	duplicate := &model.ChainHop{Name: "edge-a", Host: "b.example.net", Role: model.ChainRoleEdge,
		State: model.ChainStatePending, SubPort: 2096, SubScheme: "https"}
	if err := db.Create(duplicate).Error; err == nil {
		t.Fatal("a second hop named edge-a was accepted")
	}

	// next_hop_id is nullable — a hop whose next hop is the panel itself
	// stores NULL, not 0 (§2.2).
	inner := &model.ChainHop{Name: "inner-1", Host: "10.0.0.7", Role: model.ChainRoleInner,
		State: model.ChainStateJoined, SubPort: 2096, SubScheme: "https"}
	if err := db.Create(inner).Error; err != nil {
		t.Fatalf("create inner: %v", err)
	}
	var loaded model.ChainHop
	if err := db.First(&loaded, inner.Id).Error; err != nil {
		t.Fatal(err)
	}
	if loaded.NextHopId != nil {
		t.Fatalf("NextHopId = %v, want nil", *loaded.NextHopId)
	}
}
