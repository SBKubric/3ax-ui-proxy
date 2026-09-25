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

// monPerHopChain stores the chain the per-hop ensure tests share: an inner
// front, an active and a standby edge, and a pending edge that is not probed.
func monPerHopChain(t *testing.T) {
	t.Helper()
	monHop(t, "core-1", "inner", "joined", 0, false, "10.0.0.7")
	monHop(t, "edge-a", "edge", "joined", 0, true, "a.example.net")
	monHop(t, "edge-b", "edge", "legacy", 0, false, "b.example.net")
	monHop(t, "edge-c", "edge", "pending", 0, false, "c.example.net")
}

// monPerHopSnapshot: ams-1 on the default paths, msk-1 limited to part of
// the chain (plus names outside the probed set), ber-1 probing nothing.
func monPerHopSnapshot() []MonClient {
	return []MonClient{
		{Id: "msk-1", Paths: []string{"direct", "edge:edge-a", "edge:edge-c", "inner:nope", "proxy", "inner:edge-b"}},
		{Id: "ams-1"},
		{Id: "ber-1", Paths: []string{}},
	}
}

func monUnallocatedList(list []MonUnallocated) string {
	out := make([]string, 0, len(list))
	for _, u := range list {
		out = append(out, u.MonClientId+"/"+u.Path+"/"+u.Reason)
	}
	return strings.Join(out, ",")
}

// TestEnsureHandsPeersPerHop (contract 3 §4.3): with probed hops a peer goes
// to each pair of mon-client × path the mon-client probes — hops expands to
// every probed hop, explicit names count only inside the probed set, proxy is
// gone — created in priority order: direct, active edge, standby edges, inner.
func TestEnsureHandsPeersPerHop(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	monPerHopChain(t)

	res, err := m.EnsureProbeSet(monPerHopSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	want := "probe-awg-ams-1-direct,probe-awg-ams-1-edge-edge-a,probe-awg-ams-1-edge-edge-b,probe-awg-ams-1-inner-core-1," +
		"probe-awg-msk-1-direct,probe-awg-msk-1-edge-edge-a"
	if got := sortedKeys(awgProbePeers(t)); got != want {
		t.Errorf("peers = %s, want %s", got, want)
	}
	var order []string
	for _, c := range res.Created {
		order = append(order, c.MonClientId+"/"+c.Path)
	}
	if got := strings.Join(order, ","); got != "ams-1/direct,msk-1/direct,ams-1/edge:edge-a,msk-1/edge:edge-a,ams-1/edge:edge-b,ams-1/inner:core-1" {
		t.Errorf("created in order %s", got)
	}
	if res.Present != 6 || len(res.Unallocated) != 0 {
		t.Errorf("ensure: %+v", res)
	}

	// The standby edge leaves the probed set: its peer goes on the next ensure.
	monHopUpdate(t, "edge-b", map[string]any{"state": "draining"})
	if _, err := m.EnsureProbeSet(monPerHopSnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, ok := awgProbePeers(t)["probe-awg-ams-1-edge-edge-b"]; ok {
		t.Error("the peer of a draining hop survived ensure")
	}

	// Without probed hops the default paths mean direct and proxy again.
	database.GetDB().Where("1 = 1").Delete(&model.ChainHop{})
	if _, err := m.EnsureProbeSet(monPerHopSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(awgProbePeers(t)); got != "probe-awg-ams-1-direct,probe-awg-ams-1-proxy,probe-awg-msk-1-direct,probe-awg-msk-1-proxy" {
		t.Errorf("peers without a chain = %s", got)
	}
}

// TestEnsureRespectsThePeerLimit (contract 3 §4.3): monProbePeerLimit caps
// the peers by priority; the pairs beyond it get none, are listed as
// unallocated with reason limit and have no AWG item in /probe/configs;
// 0 lifts the cap.
func TestEnsureRespectsThePeerLimit(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	monPerHopChain(t)
	setSetting(t, "monProbePeerLimit", "3")

	res, err := m.EnsureProbeSet(monPerHopSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(awgProbePeers(t)); got != "probe-awg-ams-1-direct,probe-awg-ams-1-edge-edge-a,probe-awg-msk-1-direct" {
		t.Errorf("peers under the limit = %s", got)
	}
	if got := monUnallocatedList(res.Unallocated); got != "msk-1/edge:edge-a/limit,ams-1/edge:edge-b/limit,ams-1/inner:core-1/limit" {
		t.Errorf("unallocated = %s", got)
	}
	if res.Present != 3 {
		t.Errorf("present = %d, want 3", res.Present)
	}
	edgeA, err := m.ProbeConfigs("", "edge-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(edgeA.Items) != 1 || edgeA.Items[0].MonClientId != "ams-1" {
		t.Errorf("edge-a items: %+v, want ams-1's only", edgeA.Items)
	}

	setSetting(t, "monProbePeerLimit", "0")
	res, err = m.EnsureProbeSet(monPerHopSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unallocated) != 0 || res.Present != 6 || len(res.Created) != 3 {
		t.Errorf("ensure without a limit: %+v", res)
	}

	// Lowering the limit takes the peers of the lowest-priority pairs away.
	setSetting(t, "monProbePeerLimit", "1")
	if _, err := m.EnsureProbeSet(monPerHopSnapshot()); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(awgProbePeers(t)); got != "probe-awg-ams-1-direct" {
		t.Errorf("peers under limit 1 = %s", got)
	}
}

// TestMonClientPathsExpansion: the paths vocabulary over a probed set.
func TestMonClientPathsExpansion(t *testing.T) {
	hops := []string{"direct", "edge:edge-a", "inner:core-1"}
	plain := []string{"direct", "proxy"}
	for _, tc := range []struct {
		paths  []string
		probed []string
		want   string
	}{
		{nil, hops, "direct,edge:edge-a,inner:core-1"},
		{[]string{}, hops, ""},
		{[]string{"hops"}, hops, "edge:edge-a,inner:core-1"},
		{[]string{"direct", "proxy", "edge:nope", "inner:edge-a"}, hops, "direct"},
		{[]string{" inner:core-1 "}, hops, "inner:core-1"},
		{nil, plain, "direct,proxy"},
		{[]string{"hops"}, plain, "proxy"},
		{[]string{"proxy", "edge:edge-a"}, plain, "proxy"},
	} {
		got := monClientPaths(tc.paths, tc.probed)
		var list []string
		for _, p := range tc.probed {
			if got[p] {
				list = append(list, p)
			}
		}
		if strings.Join(list, ",") != tc.want || len(got) != len(list) {
			t.Errorf("paths %v over %v = %v, want %s", tc.paths, tc.probed, got, tc.want)
		}
	}
}

// TestHopsHealth: the badge of each hop folds all inbounds of its path over
// live mon-clients; a pending or draining hop, or one without data, is NONE;
// a probed hop is STALE while the panel is.
func TestHopsHealth(t *testing.T) {
	m := newMonitoringTestService(t)
	resetMonStaleForTest()
	if err := m.setRegistrySnapshot([]MonClient{{Id: "ams-1"}, {Id: "msk-1"}}); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	for _, row := range []model.MonTarget{
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "edge:edge-a", State: "UP"},
		{MonClientId: "msk-1", InboundKind: "awg", InboundId: 0, Path: "edge:edge-a", State: "FLAPPING"},
		{MonClientId: "gone-1", InboundKind: "xray", InboundId: 1, Path: "edge:edge-a", State: "DOWN"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "inner:core-1", State: "PAUSED"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "edge:edge-p", State: "DOWN"},
		{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "edge:core-1", State: "DOWN"},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	hops := []model.ChainHop{
		{Name: "core-1", Role: "inner", State: "joined"},
		{Name: "edge-a", Role: "edge", State: "legacy"},
		{Name: "edge-b", Role: "edge", State: "joined"},
		{Name: "edge-p", Role: "edge", State: "pending"},
		{Name: "edge-d", Role: "edge", State: "draining"},
	}
	health, err := m.HopsHealth(hops)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range health {
		got = append(got, h.Name+"/"+h.Role+"/"+h.State)
	}
	want := "core-1/inner/PAUSED,edge-a/edge/FLAPPING,edge-b/edge/NONE,edge-p/edge/NONE,edge-d/edge/NONE"
	if strings.Join(got, ",") != want {
		t.Errorf("health = %s, want %s", strings.Join(got, ","), want)
	}

	resetMonStaleForTest()
	t.Cleanup(resetMonStaleForTest)
	monStale.Lock()
	monStale.loaded, monStale.stale, monStale.since = true, true, 1
	monStale.Unlock()
	health, _ = m.HopsHealth(hops)
	if health[1].State != "STALE" || health[3].State != "NONE" {
		t.Errorf("stale panel: %+v", health)
	}
}
