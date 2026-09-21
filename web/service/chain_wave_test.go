package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// joinedWithSecret enters a hop the way a real join does and returns the hop
// secret in the clear, which is what the box presents on every poll.
func joinedWithSecret(t *testing.T, s *ChainService, in AddHopInput) (*model.ChainHop, string) {
	t.Helper()
	hop, _, _, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	secret := chain.NewSecret()
	if err := s.MarkJoined(hop.Id, chain.HashSecret(secret), ""); err != nil {
		t.Fatalf("MarkJoined(%s): %v", in.Name, err)
	}
	reloaded := hopByName(t, s, in.Name)
	return &reloaded, secret
}

func TestChainWaveAuthenticatesAFirstTierHop(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	inner, secret := joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})

	hop, ok := wave.AuthenticateHop(secret)
	if !ok || hop.Id != inner.Id {
		t.Fatalf("AuthenticateHop(hop secret) = %v, %v; want inner-1", hop, ok)
	}
}

func TestChainWaveRefusesEverythingElse(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	_, innerSecret := joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	// An edge hangs off the inner, so it never talks to the panel: its next
	// hop is a box, not us.
	_, edgeSecret := joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	pending, _ := pendingHop(t, registry, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	pendingSecret := chain.NewSecret()
	if err := database.GetDB().Model(&model.ChainHop{}).Where("id = ?", pending.Id).
		Update("secret_hash", chain.HashSecret(pendingSecret)).Error; err != nil {
		t.Fatalf("plant a secret on a pending hop: %v", err)
	}

	for name, bearer := range map[string]string{
		"an empty bearer":         "",
		"a secret nobody holds":   chain.NewSecret(),
		"the hop name":            "inner-1",
		"a second-tier hop":       edgeSecret,
		"a hop that never joined": pendingSecret,
		"the secret with junk":    innerSecret + "x",
	} {
		if hop, ok := wave.AuthenticateHop(bearer); ok {
			t.Fatalf("AuthenticateHop with %s let %q in", name, hop.Name)
		}
	}
}

func TestChainWaveRecordsTheCallersPoll(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	before := revisionOf(t, registry)

	if err := wave.RecordSeen("inner-1", before, nil); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	stored := hopByName(t, registry, "inner-1")
	if stored.LastRevision != before {
		t.Fatalf("lastRevision is %d, want %d", stored.LastRevision, before)
	}
	if stored.LastSeenAt == 0 || stored.LastSeenAt > time.Now().UnixMilli() {
		t.Fatalf("lastSeenAt is %d", stored.LastSeenAt)
	}
	if after := revisionOf(t, registry); after != before {
		t.Fatalf("a poll moved the chain revision %d → %d", before, after)
	}
}

func TestChainWaveRecordsOuterAcknowledgements(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	future := time.Now().Add(time.Hour).UnixMilli()
	revision := revisionOf(t, registry)

	err := wave.RecordSeen("inner-1", revision, []chain.OuterAck{
		{Name: "edge-a", LastRevision: revision, LastSeen: future},
		{Name: "ghost", LastRevision: revision, LastSeen: future},
	})
	if err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	edge := hopByName(t, registry, "edge-a")
	if edge.LastRevision != revision {
		t.Fatalf("edge-a lastRevision is %d, want %d", edge.LastRevision, revision)
	}
	if edge.LastSeenAt > time.Now().UnixMilli() {
		t.Fatalf("edge-a lastSeenAt %d is in the future", edge.LastSeenAt)
	}
	if edge.LastSeenAt == 0 {
		t.Fatal("edge-a lastSeenAt was not recorded")
	}
}

func TestChainWaveNeverWindsAHopBack(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	revision := revisionOf(t, registry)
	if err := wave.RecordSeen("inner-1", revision, []chain.OuterAck{{Name: "edge-a", LastRevision: revision, LastSeen: time.Now().UnixMilli()}}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	seenAt := hopByName(t, registry, "edge-a").LastSeenAt

	// A stale acknowledgement travelling behind a fresher one must not undo it.
	if err := wave.RecordSeen("inner-1", revision, []chain.OuterAck{{Name: "edge-a", LastRevision: revision - 1, LastSeen: seenAt - 10_000}}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	edge := hopByName(t, registry, "edge-a")
	if edge.LastRevision != revision {
		t.Fatalf("edge-a lastRevision fell back to %d, want %d", edge.LastRevision, revision)
	}
	if edge.LastSeenAt < seenAt {
		t.Fatalf("edge-a lastSeenAt fell back from %d to %d", seenAt, edge.LastSeenAt)
	}
}

func TestChainWaveIgnoresAnAcknowledgementFromInward(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-2", Host: "203.0.113.9", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	state, err := registry.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// inner-2 may speak for the edge outward of it and for nobody inward: a
	// seized box must not be able to report freshness it cannot have seen.
	err = wave.RecordSeen("inner-2", state.Revision, []chain.OuterAck{
		{Name: "inner-1", LastRevision: state.Revision, LastSeen: time.Now().UnixMilli()},
		{Name: "edge-a", LastRevision: state.Revision, LastSeen: time.Now().UnixMilli()},
	})
	if err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	if inner := hopByName(t, registry, "inner-1"); inner.LastRevision != 0 {
		t.Fatalf("an acknowledgement from inward was recorded: %+v", inner)
	}
	if edge := hopByName(t, registry, "edge-a"); edge.LastRevision != state.Revision {
		t.Fatalf("the acknowledgement for the hop outward was not recorded: %+v", edge)
	}
}

func TestChainWaveIgnoresAnEdgesAcknowledgementOfItsNeighbour(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	state, _ := registry.List()

	if err := wave.RecordSeen("edge-a", state.Revision, []chain.OuterAck{
		{Name: "edge-b", LastRevision: state.Revision, LastSeen: time.Now().UnixMilli()},
	}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	if edge := hopByName(t, registry, "edge-b"); edge.LastRevision != 0 {
		t.Fatalf("an edge spoke for the edge beside it: %+v", edge)
	}
}

func TestChainWaveClampsARevisionToTheRegistrys(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	state, _ := registry.List()

	err := wave.RecordSeen("inner-1", state.Revision+50, []chain.OuterAck{
		{Name: "edge-a", LastRevision: state.Revision + 100, LastSeen: time.Now().UnixMilli()},
	})
	if err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	if inner := hopByName(t, registry, "inner-1"); inner.LastRevision != state.Revision {
		t.Fatalf("the caller's revision %d was not clamped to %d", inner.LastRevision, state.Revision)
	}
	if edge := hopByName(t, registry, "edge-a"); edge.LastRevision != state.Revision {
		t.Fatalf("the acknowledged revision %d was not clamped to %d", edge.LastRevision, state.Revision)
	}
}

// §3.3, §4.5.3 — a first-tier hop on its way out still authenticates. It has
// to receive at least one more document — the one in which its own self.state
// became draining — or it would go on naming itself to its neighbours, which
// is exactly the freeze the stand (#86) ran into.
func TestAuthenticateHopAdmitsADrainingFirstTierHop(t *testing.T) {
	registry := newChainService(t)
	wave := &ChainWaveService{}
	hop, secret := joinedWithSecret(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	joinedWithSecret(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := registry.Delete(hop.Id, false, false); err != nil {
		t.Fatalf("Delete(inner-1): %v", err)
	}
	draining := hopByName(t, registry, "inner-1")
	if draining.State != chain.StateDraining {
		t.Fatalf("inner-1 is %q, want draining", draining.State)
	}

	authenticated, ok := wave.AuthenticateHop(secret)
	if !ok {
		t.Fatal("a draining first-tier hop must still be let in")
	}
	if authenticated.Name != "inner-1" {
		t.Errorf("authenticated %q, want inner-1", authenticated.Name)
	}
}
