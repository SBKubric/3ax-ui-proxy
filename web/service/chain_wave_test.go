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

	if err := wave.RecordSeen("inner-1", 7, nil); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	stored := hopByName(t, registry, "inner-1")
	if stored.LastRevision != 7 {
		t.Fatalf("lastRevision is %d, want 7", stored.LastRevision)
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

	err := wave.RecordSeen("inner-1", 7, []chain.OuterAck{
		{Name: "edge-a", LastRevision: 6, LastSeen: future},
		{Name: "ghost", LastRevision: 99, LastSeen: future},
	})
	if err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	edge := hopByName(t, registry, "edge-a")
	if edge.LastRevision != 6 {
		t.Fatalf("edge-a lastRevision is %d, want 6", edge.LastRevision)
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
	if err := wave.RecordSeen("inner-1", 9, []chain.OuterAck{{Name: "edge-a", LastRevision: 9, LastSeen: time.Now().UnixMilli()}}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	seenAt := hopByName(t, registry, "edge-a").LastSeenAt

	// A stale acknowledgement travelling behind a fresher one must not undo it.
	if err := wave.RecordSeen("inner-1", 9, []chain.OuterAck{{Name: "edge-a", LastRevision: 4, LastSeen: seenAt - 10_000}}); err != nil {
		t.Fatalf("RecordSeen: %v", err)
	}
	edge := hopByName(t, registry, "edge-a")
	if edge.LastRevision != 9 {
		t.Fatalf("edge-a lastRevision fell back to %d", edge.LastRevision)
	}
	if edge.LastSeenAt < seenAt {
		t.Fatalf("edge-a lastSeenAt fell back from %d to %d", seenAt, edge.LastSeenAt)
	}
}
