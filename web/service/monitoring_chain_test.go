package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// monHop stores a registry row as the chain editor would have left it.
func monHop(t *testing.T, name, role, state string, position int, active bool, host string) *model.ChainHop {
	t.Helper()
	hop := &model.ChainHop{Name: name, Role: role, State: state, Position: position, IsActive: active, Host: host,
		SubPort: 2096, SubScheme: "https"}
	if err := database.GetDB().Create(hop).Error; err != nil {
		t.Fatalf("create hop %s: %v", name, err)
	}
	return hop
}

// monHopUpdate changes columns of a stored hop.
func monHopUpdate(t *testing.T, name string, updates map[string]any) {
	t.Helper()
	if err := database.GetDB().Model(&model.ChainHop{}).Where("name = ?", name).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
}

// TestStateCarriesTheChain (contract 3 §4.1): no chain field with an empty
// registry; otherwise the registry revision, the active edge and only the
// joined and legacy hops — inner fronts by position, then edges by name.
func TestStateCarriesTheChain(t *testing.T) {
	m := newMonitoringTestService(t)
	st, err := m.State()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if st.Chain != nil || strings.Contains(string(raw), `"chain"`) {
		t.Errorf("empty registry: chain = %s", raw)
	}

	monHop(t, "edge-b", "edge", "joined", 0, false, "b.example.net")
	monHop(t, "inner-2", "inner", "joined", 1, false, "10.0.0.8")
	monHop(t, "edge-a", "edge", "joined", 0, true, "a.example.net")
	monHop(t, "inner-1", "inner", "legacy", 0, false, "10.0.0.7")
	monHop(t, "edge-c", "edge", "pending", 0, false, "c.example.net")
	monHop(t, "inner-0", "inner", "draining", 0, false, "10.0.0.6")
	setSetting(t, "chainRevision", "42")

	st, err = m.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.Contract != 3 || st.Chain == nil || st.Chain.Revision != 42 || st.Chain.ActiveEdge == nil || *st.Chain.ActiveEdge != "edge-a" {
		t.Fatalf("chain header: %+v", st.Chain)
	}
	var names []string
	for _, h := range st.Chain.Hops {
		names = append(names, h.Name+"/"+h.Role+"/"+h.State+"/"+h.Host)
	}
	want := "inner-1/inner/legacy/10.0.0.7,inner-2/inner/joined/10.0.0.8,edge-a/edge/joined/a.example.net,edge-b/edge/joined/b.example.net"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("hops = %s, want %s", got, want)
	}

	// The active edge back in pending after reissueToken still holds the
	// override, so it stays activeEdge — but it is not probed.
	monHopUpdate(t, "edge-a", map[string]any{"state": "pending"})
	st, _ = m.State()
	if st.Chain.ActiveEdge == nil || *st.Chain.ActiveEdge != "edge-a" || len(st.Chain.Hops) != 3 {
		t.Errorf("pending active edge: %+v", st.Chain)
	}

	// No active edge: null on the wire.
	monHopUpdate(t, "edge-a", map[string]any{"is_active": false})
	st, _ = m.State()
	raw, _ = json.Marshal(st.Chain)
	if !strings.Contains(string(raw), `"activeEdge":null`) {
		t.Errorf("no active edge: %s", raw)
	}
}

// TestRevisionTracksTheChain (contract 3 §4.2): the active edge and the
// probed hops move the revision — a join, a rename, a host, a switch of the
// active edge — while a pending hop and the registry revision alone do not.
func TestRevisionTracksTheChain(t *testing.T) {
	m := newMonitoringTestService(t)
	rev := monRevision(t, m)
	empty := rev()

	monHop(t, "edge-a", "edge", "pending", 0, false, "a.example.net")
	withPending := rev()
	if withPending != empty {
		t.Error("a registry of pending hops moved the revision")
	}
	setSetting(t, "chainRevision", "7")
	if rev() != withPending {
		t.Error("the registry revision alone moved the revision")
	}
	monHop(t, "edge-b", "edge", "pending", 0, false, "b.example.net")
	if rev() != withPending {
		t.Error("adding a pending hop moved the revision")
	}

	monHopUpdate(t, "edge-a", map[string]any{"state": "joined"})
	joined := rev()
	if joined == withPending {
		t.Error("a hop joining left the revision unchanged")
	}
	monHopUpdate(t, "edge-a", map[string]any{"is_active": true})
	active := rev()
	if active == joined {
		t.Error("switching the active edge left the revision unchanged")
	}
	monHopUpdate(t, "edge-a", map[string]any{"host": "a2.example.net"})
	moved := rev()
	if moved == active {
		t.Error("a new host of a probed hop left the revision unchanged")
	}
	monHopUpdate(t, "edge-a", map[string]any{"name": "edge-z"})
	if rev() == moved {
		t.Error("renaming a probed hop left the revision unchanged")
	}
}

// TestMonProbedPathsOrder: the probed set in priority order — direct, the
// active edge, standby edges by name, inner fronts outward — and direct plus
// proxy while no hop is probed.
func TestMonProbedPathsOrder(t *testing.T) {
	if got := strings.Join(monProbedPaths(nil), ","); got != "direct,proxy" {
		t.Errorf("no chain: %s", got)
	}
	if got := strings.Join(monProbedPaths(&MonChain{Hops: []MonChainHop{}}), ","); got != "direct,proxy" {
		t.Errorf("no probed hops: %s", got)
	}
	active := "edge-b"
	c := &MonChain{ActiveEdge: &active, Hops: []MonChainHop{
		{Name: "core-1", Role: "inner"}, {Name: "core-2", Role: "inner"},
		{Name: "edge-a", Role: "edge"}, {Name: "edge-b", Role: "edge"}, {Name: "edge-c", Role: "edge"},
	}}
	want := "direct,edge:edge-b,edge:edge-a,edge:edge-c,inner:core-1,inner:core-2"
	if got := strings.Join(monProbedPaths(c), ","); got != want {
		t.Errorf("paths = %s, want %s", got, want)
	}
}
