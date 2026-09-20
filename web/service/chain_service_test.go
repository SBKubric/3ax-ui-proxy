package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

func newChainService(t *testing.T) *ChainService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	return &ChainService{}
}

func revisionOf(t *testing.T, s *ChainService) int64 {
	t.Helper()
	state, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return state.Revision
}

func hopByName(t *testing.T, s *ChainService, name string) model.ChainHop {
	t.Helper()
	state, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, hop := range state.Hops {
		if hop.Name == name {
			return hop
		}
	}
	t.Fatalf("hop %q not in the registry", name)
	return model.ChainHop{}
}

// addHop creates a pending hop the way the owner would, and fails the test if
// the registry refuses it.
func addHop(t *testing.T, s *ChainService, in AddHopInput) *model.ChainHop {
	t.Helper()
	hop, token, expires, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	if len(token) != chain.SecretLength {
		t.Fatalf("join token %q is not %d characters", token, chain.SecretLength)
	}
	if chain.HashSecret(token) != hop.JoinTokenHash {
		t.Fatal("the stored join token hash is not the hash of the returned token")
	}
	if expires <= time.Now().UnixMilli() {
		t.Fatalf("join token expires at %d, which is not in the future", expires)
	}
	return hop
}

// joinHop is what ticket #81's join flow does to a pending hop; the registry
// side of it lives here, so the tests drive it through MarkJoined.
func joinHop(t *testing.T, s *ChainService, hop *model.ChainHop) {
	t.Helper()
	if err := s.MarkJoined(hop.Id, chain.HashSecret(chain.NewSecret()), "198.51.100.7"); err != nil {
		t.Fatalf("MarkJoined(%s): %v", hop.Name, err)
	}
}

func addJoined(t *testing.T, s *ChainService, in AddHopInput) *model.ChainHop {
	t.Helper()
	hop := addHop(t, s, in)
	joinHop(t, s, hop)
	reloaded := hopByName(t, s, hop.Name)
	return &reloaded
}

func nextHopName(t *testing.T, s *ChainService, name string) string {
	t.Helper()
	hop := hopByName(t, s, name)
	if hop.NextHopId == nil {
		return ""
	}
	state, _ := s.List()
	for _, candidate := range state.Hops {
		if candidate.Id == *hop.NextHopId {
			return candidate.Name
		}
	}
	t.Fatalf("hop %q points at id %d, which is not in the registry", name, *hop.NextHopId)
	return ""
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	if got := ChainErrorCode(err); got != code {
		t.Fatalf("error code %q, want %q (err: %v)", got, code, err)
	}
}

// §2.6.1 — a chain of one. The single edge hangs off the panel itself, and
// once it is active the panel publishes its host instead of the real server's.
func TestChainScenarioSingleEdge(t *testing.T) {
	s := newChainService(t)

	edge := addHop(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if edge.State != chain.StatePending {
		t.Fatalf("a new hop starts in state %q, want pending", edge.State)
	}
	if edge.NextHopId != nil {
		t.Fatal("with no inner fronts the edge's next hop is the panel, so next_hop_id must be NULL")
	}
	if revisionOf(t, s) != 0 {
		t.Fatal("a pending hop changes no document, so it must not move the revision (§3.4)")
	}

	// A pending hop cannot carry the override: no box has entered yet.
	wantCode(t, s.SetActive(edge.Id), CodeHopNotJoined)
	if _, ok := s.ActiveEdgeHost(); ok {
		t.Fatal("a pending edge must not become the published host")
	}

	joinHop(t, s, edge)
	if revisionOf(t, s) != 1 {
		t.Fatalf("join must move the revision, got %d", revisionOf(t, s))
	}
	if err := s.SetActive(edge.Id); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if revisionOf(t, s) != 2 {
		t.Fatalf("switching the active edge must move the revision, got %d", revisionOf(t, s))
	}
	host, ok := s.ActiveEdgeHost()
	if !ok || host != "a.example.net" {
		t.Fatalf("ActiveEdgeHost = %q, %v; want a.example.net, true", host, ok)
	}

	state, _ := s.List()
	if state.ActiveEdge != "edge-a" || state.PollSeconds != 30 {
		t.Fatalf("List = %+v", state)
	}
}

// §2.6.2 — two standby edges. All three edges hang off the last inner, exactly
// one is active, and switching is a single registry write.
func TestChainScenarioStandbyEdges(t *testing.T) {
	s := newChainService(t)

	inner := addJoined(t, s, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	edgeA := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	edgeB := addJoined(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	edgeC := addJoined(t, s, AddHopInput{Name: "edge-c", Host: "c.example.net", Role: chain.RoleEdge})

	for _, edge := range []*model.ChainHop{edgeA, edgeB, edgeC} {
		if got := nextHopName(t, s, edge.Name); got != inner.Name {
			t.Errorf("%s hangs off %q, want %q", edge.Name, got, inner.Name)
		}
	}

	if err := s.SetActive(edgeA.Id); err != nil {
		t.Fatal(err)
	}
	before := revisionOf(t, s)
	if err := s.SetActive(edgeB.Id); err != nil {
		t.Fatal(err)
	}
	if revisionOf(t, s) != before+1 {
		t.Fatal("switching the active edge must move the revision exactly once")
	}

	state, _ := s.List()
	active := 0
	for _, hop := range state.Hops {
		if hop.IsActive {
			active++
			if hop.Name != "edge-b" {
				t.Errorf("active hop is %q, want edge-b", hop.Name)
			}
		}
	}
	if active != 1 {
		t.Fatalf("%d active hops, want exactly 1 (invariant 1)", active)
	}
	if state.ActiveEdge != "edge-b" {
		t.Errorf("activeEdge = %q, want edge-b", state.ActiveEdge)
	}
	host, _ := s.ActiveEdgeHost()
	if host != "b.example.net" {
		t.Errorf("published host = %q, want b.example.net", host)
	}

	// Switching to the edge that is already active changes no document.
	before = revisionOf(t, s)
	if err := s.SetActive(edgeB.Id); err != nil {
		t.Fatal(err)
	}
	if revisionOf(t, s) != before {
		t.Error("re-activating the active edge must not move the revision")
	}
}

// §2.6.3 — inserting an inner between two existing ones. The neighbours are
// re-chained and the positions shift inside one transaction, and the revision
// waits for the join: until the new box is really there, no document may point
// at it.
func TestChainScenarioInsertInner(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	edge := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if got := nextHopName(t, s, edge.Name); got != "b" {
		t.Fatalf("the edge hangs off %q, want the last inner b", got)
	}

	before := revisionOf(t, s)
	position := 1
	m := addHop(t, s, AddHopInput{Name: "m", Host: "10.0.0.3", Role: chain.RoleInner, Position: &position})
	if revisionOf(t, s) != before {
		t.Fatal("adding a pending inner must not move the revision (§2.6.3)")
	}

	if got := nextHopName(t, s, "m"); got != "a" {
		t.Errorf("m's next hop is %q, want a", got)
	}
	if got := nextHopName(t, s, "b"); got != "m" {
		t.Errorf("b's next hop is %q, want m", got)
	}
	if got := nextHopName(t, s, "a"); got != "" {
		t.Errorf("a's next hop is %q, want the panel (NULL)", got)
	}
	if got := hopByName(t, s, "b").Position; got != 2 {
		t.Errorf("b sits at position %d, want 2", got)
	}
	if got := hopByName(t, s, "m").Position; got != 1 {
		t.Errorf("m sits at position %d, want 1", got)
	}
	if got := hopByName(t, s, "a").Position; got != 0 {
		t.Errorf("a sits at position %d, want 0", got)
	}
	// The edge still hangs off the last *joined* inner while m is pending.
	if got := nextHopName(t, s, edge.Name); got != "b" {
		t.Errorf("while m is pending the edge hangs off %q, want b", got)
	}

	joinHop(t, s, m)
	if revisionOf(t, s) != before+1 {
		t.Fatal("the join is the write that moves the revision")
	}

}

// §2.6.4 — the active edge is not deleted by accident. force is the
// decommissioning path and only when nothing else could carry the override.
func TestChainScenarioDeleteActiveEdge(t *testing.T) {
	s := newChainService(t)

	edgeA := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	edgeB := addJoined(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	if err := s.SetActive(edgeA.Id); err != nil {
		t.Fatal(err)
	}

	_, err := s.Delete(edgeA.Id, false)
	wantCode(t, err, CodeActiveEdgeInUse)

	// force is refused too while another joined edge exists: the owner is
	// meant to switch over first, and an automatic hand-over would be a
	// failover, which is out of scope.
	_, err = s.Delete(edgeA.Id, true)
	wantCode(t, err, CodeActiveEdgeInUse)

	if _, err := s.Delete(edgeB.Id, false); err != nil {
		t.Fatalf("deleting a standby edge: %v", err)
	}
	if _, err := s.Delete(edgeA.Id, true); err != nil {
		t.Fatalf("force-deleting the last edge: %v", err)
	}
	if _, ok := s.ActiveEdgeHost(); ok {
		t.Fatal("with the last edge gone the override must be off — the panel publishes the real server")
	}
	state, _ := s.List()
	if len(state.Hops) != 0 {
		t.Fatalf("registry still holds %+v", state.Hops)
	}
}

// §4.5 — deleting a hop re-chains its outer neighbour onto its own next hop,
// compacts the positions, and hands back the hop and revision the owner must
// wait for before powering the box off.
func TestChainDeleteRechainsAndCompacts(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "c", Host: "10.0.0.3", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	before := revisionOf(t, s)
	result, err := s.Delete(b.Id, false)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if revisionOf(t, s) != before+1 {
		t.Error("deleting a hop changes documents, so it must move the revision")
	}
	if result.SafeToPowerOffWhen.Hop != "c" || result.SafeToPowerOffWhen.Revision != before+1 {
		t.Errorf("safeToPowerOffWhen = %+v, want {c %d}", result.SafeToPowerOffWhen, before+1)
	}

	if got := nextHopName(t, s, "c"); got != "a" {
		t.Errorf("c's next hop is %q, want a", got)
	}
	if got := hopByName(t, s, "c").Position; got != 1 {
		t.Errorf("c sits at position %d, want 1 after compaction", got)
	}
	if got := nextHopName(t, s, "edge-a"); got != "c" {
		t.Errorf("the edge hangs off %q, want the last inner c", got)
	}

	// Deleting the last inner re-chains every edge hanging off it.
	c := hopByName(t, s, "c")
	if _, err := s.Delete(c.Id, false); err != nil {
		t.Fatalf("Delete c: %v", err)
	}
	if got := nextHopName(t, s, "edge-a"); got != "a" {
		t.Errorf("the edge hangs off %q, want a", got)
	}

	a := hopByName(t, s, "a")
	if _, err := s.Delete(a.Id, false); err != nil {
		t.Fatalf("Delete a: %v", err)
	}
	if got := nextHopName(t, s, "edge-a"); got != "" {
		t.Errorf("with no inners left the edge hangs off %q, want the panel", got)
	}
}

// §2.7 — the invariants are refusals with stable codes, not silent repairs.
func TestChainInvariants(t *testing.T) {
	s := newChainService(t)

	inner := addJoined(t, s, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	edge := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	pending := addHop(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})

	t.Run("name taken", func(t *testing.T) {
		_, _, _, err := s.Add(AddHopInput{Name: "edge-a", Host: "x.example.net", Role: chain.RoleEdge})
		wantCode(t, err, CodeNameTaken)
	})
	t.Run("invalid name", func(t *testing.T) {
		for _, name := range []string{"", "Edge", "edge_a", "край", "this-name-is-far-too-long-to-fit-in-32"} {
			_, _, _, err := s.Add(AddHopInput{Name: name, Host: "x.example.net", Role: chain.RoleEdge})
			wantCode(t, err, CodeInvalidName)
		}
	})
	t.Run("invalid role", func(t *testing.T) {
		_, _, _, err := s.Add(AddHopInput{Name: "weird", Host: "x.example.net", Role: "middle"})
		wantCode(t, err, CodeInvalidRole)
	})
	t.Run("invalid host", func(t *testing.T) {
		_, _, _, err := s.Add(AddHopInput{Name: "hostless", Host: "   ", Role: chain.RoleEdge})
		wantCode(t, err, CodeInvalidHost)
	})
	t.Run("invalid sub port", func(t *testing.T) {
		_, _, _, err := s.Add(AddHopInput{Name: "porty", Host: "x.example.net", Role: chain.RoleEdge, SubPort: 70000})
		wantCode(t, err, CodeInvalidSubPort)
	})
	t.Run("invalid sub scheme", func(t *testing.T) {
		_, _, _, err := s.Add(AddHopInput{Name: "schemey", Host: "x.example.net", Role: chain.RoleEdge, SubScheme: "ftp"})
		wantCode(t, err, CodeInvalidSubScheme)
	})
	t.Run("invalid position", func(t *testing.T) {
		position := 9
		_, _, _, err := s.Add(AddHopInput{Name: "toofar", Host: "x.example.net", Role: chain.RoleInner, Position: &position})
		wantCode(t, err, CodeInvalidPosition)
	})
	t.Run("unknown hop", func(t *testing.T) {
		wantCode(t, s.SetActive(4242), CodeUnknownHop)
		_, err := s.Delete(4242, false)
		wantCode(t, err, CodeUnknownHop)
		wantCode(t, s.Update(4242, UpdateHopInput{}), CodeUnknownHop)
		_, _, err = s.ReissueToken(4242)
		wantCode(t, err, CodeUnknownHop)
	})
	t.Run("not an edge", func(t *testing.T) {
		wantCode(t, s.SetActive(inner.Id), CodeNotAnEdge)
	})
	t.Run("pending cannot be active", func(t *testing.T) {
		wantCode(t, s.SetActive(pending.Id), CodeHopNotJoined)
	})
	t.Run("rename collision", func(t *testing.T) {
		name := "edge-a"
		wantCode(t, s.Update(pending.Id, UpdateHopInput{Name: &name}), CodeNameTaken)
	})
	t.Run("rename to its own name is allowed", func(t *testing.T) {
		name := "edge-a"
		if err := s.Update(edge.Id, UpdateHopInput{Name: &name}); err != nil {
			t.Fatalf("renaming a hop to what it is called already: %v", err)
		}
	})
	t.Run("single inner path", func(t *testing.T) {
		second := addJoined(t, s, AddHopInput{Name: "inner-2", Host: "10.0.0.8", Role: chain.RoleInner})
		if got := nextHopName(t, s, second.Name); got != "inner-1" {
			t.Errorf("inner-2's next hop is %q, want inner-1", got)
		}
		state, _ := s.List()
		roots, positions := 0, map[int]int{}
		for _, hop := range state.Hops {
			if hop.Role != chain.RoleInner {
				continue
			}
			if hop.NextHopId == nil {
				roots++
			}
			positions[hop.Position]++
		}
		if roots != 1 {
			t.Errorf("%d inner fronts point at the panel, want exactly 1 (invariant 3)", roots)
		}
		for position, count := range positions {
			if count != 1 {
				t.Errorf("position %d is held by %d hops, want 1", position, count)
			}
		}
	})
}

// §3.4 — which writes move the revision and which do not. The matrix is the
// whole contract of the wave: a missed bump leaves boxes on a stale document,
// a spurious one restarts relays for nothing.
func TestChainRevisionBumpMatrix(t *testing.T) {
	tests := []struct {
		name  string
		do    func(t *testing.T, s *ChainService)
		bumps int
	}{
		{"add pending hop", func(t *testing.T, s *ChainService) {
			addHop(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
		}, 0},
		{"join", func(t *testing.T, s *ChainService) {
			hop := addHop(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
			joinHop(t, s, hop)
		}, 1},
		{"reissue token", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			if _, _, err := s.ReissueToken(hop.Id); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"change host", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			host := "moved.example.net"
			if err := s.Update(hop.Id, UpdateHopInput{Host: &host}); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"rename", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			name := "edge-z"
			if err := s.Update(hop.Id, UpdateHopInput{Name: &name}); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"change sub port", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			port := 8443
			if err := s.Update(hop.Id, UpdateHopInput{SubPort: &port}); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"update with nothing to change", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			host := hop.Host
			if err := s.Update(hop.Id, UpdateHopInput{Host: &host}); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"delete", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "inner-1")
			if _, err := s.Delete(hop.Id, false); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"set active", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			if err := s.SetActive(hop.Id); err != nil {
				t.Fatal(err)
			}
		}, 1},
		{"clear active when nothing is active", func(t *testing.T, s *ChainService) {
			if err := s.ClearActive(); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"clear active after activating", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			if err := s.SetActive(hop.Id); err != nil {
				t.Fatal(err)
			}
			if err := s.ClearActive(); err != nil {
				t.Fatal(err)
			}
		}, 2},
		{"record poll", func(t *testing.T, s *ChainService) {
			hop := hopByName(t, s, "edge-a")
			if err := s.RecordSeen(hop.Id, 42); err != nil {
				t.Fatal(err)
			}
		}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newChainService(t)
			addJoined(t, s, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
			addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

			before := revisionOf(t, s)
			tt.do(t, s)
			after := revisionOf(t, s)

			if after != before+int64(tt.bumps) {
				t.Fatalf("revision went %d -> %d, want %d bump(s)", before, after, tt.bumps)
			}
		})
	}
}

// §4.1, §4.4 — a reissued token replaces the old one on the same hop: same id,
// same name, same place in the chain, and the old token is dead at once. A
// joined hop goes back to pending, because the box has to enter again.
func TestChainReissueToken(t *testing.T) {
	s := newChainService(t)

	inner := addJoined(t, s, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	first := hopByName(t, s, inner.Name)
	firstHash := first.JoinTokenHash

	token, expires, err := s.ReissueToken(inner.Id)
	if err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}
	if len(token) != chain.SecretLength {
		t.Fatalf("token %q is not %d characters", token, chain.SecretLength)
	}
	if expires <= time.Now().UnixMilli() {
		t.Fatalf("expiry %d is not in the future", expires)
	}

	after := hopByName(t, s, inner.Name)
	if after.Id != inner.Id || after.Name != inner.Name || after.Host != inner.Host || after.Position != inner.Position {
		t.Fatalf("the hop changed: %+v, was %+v", after, inner)
	}
	if after.JoinTokenHash != chain.HashSecret(token) {
		t.Fatal("the stored hash is not the hash of the returned token")
	}
	if after.JoinTokenHash == firstHash {
		t.Fatal("the old token hash survived, so the old token would still work")
	}
	if after.State != chain.StatePending {
		t.Fatalf("state after a reissue is %q, want pending — the box must enter again (§4.2)", after.State)
	}
	if after.SecretHash == "" {
		t.Fatal("the hop secret must survive until the new box really joins (§4.4)")
	}
}

// §2.3 — the legacy proxyOverrideHost becomes one hop named legacy, and the
// migration may run on every start.
func TestChainMigrateLegacyOverride(t *testing.T) {
	t.Run("imports an enabled override", func(t *testing.T) {
		s := newChainService(t)
		setSetting(t, "proxyOverrideEnable", "true")
		setSetting(t, "proxyOverrideHost", "front.example.net")

		if err := s.MigrateLegacyOverride(); err != nil {
			t.Fatalf("MigrateLegacyOverride: %v", err)
		}
		hop := hopByName(t, s, "legacy")
		if hop.Role != chain.RoleEdge || hop.State != chain.StateLegacy || hop.Host != "front.example.net" {
			t.Fatalf("imported hop = %+v", hop)
		}
		if !hop.IsActive {
			t.Fatal("an enabled override must arrive as the active edge")
		}
		host, ok := s.ActiveEdgeHost()
		if !ok || host != "front.example.net" {
			t.Fatalf("ActiveEdgeHost = %q, %v", host, ok)
		}

		// Idempotent: a second run must not create a second hop, whatever the
		// legacy keys still say.
		revision := revisionOf(t, s)
		if err := s.MigrateLegacyOverride(); err != nil {
			t.Fatalf("second MigrateLegacyOverride: %v", err)
		}
		state, _ := s.List()
		if len(state.Hops) != 1 {
			t.Fatalf("%d hops after a second migration, want 1", len(state.Hops))
		}
		if revisionOf(t, s) != revision {
			t.Error("a no-op migration must not move the revision")
		}
	})

	t.Run("imports a disabled override as a standby", func(t *testing.T) {
		s := newChainService(t)
		setSetting(t, "proxyOverrideHost", "front.example.net")

		if err := s.MigrateLegacyOverride(); err != nil {
			t.Fatal(err)
		}
		hop := hopByName(t, s, "legacy")
		if hop.IsActive {
			t.Fatal("a disabled override must not arrive active — the panel was publishing the real server")
		}
		if _, ok := s.ActiveEdgeHost(); ok {
			t.Fatal("ActiveEdgeHost must stay empty")
		}
	})

	t.Run("does nothing without a legacy host", func(t *testing.T) {
		s := newChainService(t)
		if err := s.MigrateLegacyOverride(); err != nil {
			t.Fatal(err)
		}
		state, _ := s.List()
		if len(state.Hops) != 0 || state.Revision != 0 {
			t.Fatalf("empty panel got %+v", state)
		}
	})

	t.Run("does nothing when the registry is already in use", func(t *testing.T) {
		s := newChainService(t)
		addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
		setSetting(t, "proxyOverrideHost", "front.example.net")

		if err := s.MigrateLegacyOverride(); err != nil {
			t.Fatal(err)
		}
		state, _ := s.List()
		if len(state.Hops) != 1 || state.Hops[0].Name != "edge-a" {
			t.Fatalf("migration touched a registry that was already in use: %+v", state.Hops)
		}
	})
}
