package service

import (
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// hopExists says whether the registry still holds a row under that name.
func hopExists(t *testing.T, s *ChainService, name string) bool {
	t.Helper()
	state, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, hop := range state.Hops {
		if hop.Name == name {
			return true
		}
	}
	return false
}

// confirmRevision is what a poll does for a hop: it writes down the revision
// that hop has applied (ChainWaveService.RecordSeen, §3.3).
func confirmRevision(t *testing.T, name string, revision int64) {
	t.Helper()
	err := database.GetDB().Model(&model.ChainHop{}).Where("name = ?", name).
		Updates(map[string]any{"last_revision": revision, "last_seen_at": time.Now().UnixMilli()}).Error
	if err != nil {
		t.Fatalf("confirming revision %d for %s: %v", revision, name, err)
	}
}

// expireDrain drags a departure's deadline into the past, which is how a test
// reaches the timeout branch without waiting ten minutes.
func expireDrain(t *testing.T, name string) {
	t.Helper()
	err := database.GetDB().Model(&model.ChainHop{}).Where("name = ?", name).
		Update("drain_until", time.Now().Add(-time.Minute).UnixMilli()).Error
	if err != nil {
		t.Fatalf("expiring the drain deadline of %s: %v", name, err)
	}
}

// §4.5.1 — deleting an inner that still carries neighbours does not delete it.
// The row stays as draining, keeping the host, the secret and the next hop it
// had, while the neighbours are re-chained past it in the same revision. This
// is the stand's bug (#86) from the registry's side: killing the row at once
// would cut the only channel that can carry that revision outward.
func TestChainDeleteStartsDraining(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	addJoined(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})

	before := revisionOf(t, s)
	result, err := s.Delete(b.Id, false, false)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if result.State != DeleteStateDraining {
		t.Fatalf("state %q, want %q", result.State, DeleteStateDraining)
	}
	if result.Hop != "b" {
		t.Errorf("hop %q, want b", result.Hop)
	}
	if revisionOf(t, s) != before+1 {
		t.Errorf("the start of a departure moves the revision exactly once, got %d", revisionOf(t, s))
	}
	if result.DrainRevision != before+1 {
		t.Errorf("drainRevision %d, want %d", result.DrainRevision, before+1)
	}
	if result.DrainUntil <= time.Now().UnixMilli() {
		t.Errorf("drainUntil %d is not in the future", result.DrainUntil)
	}
	// Both former neighbours have to confirm, not whichever the panel picked:
	// the box is unsafe to power off while any of them still relays into it.
	if got := result.SafeToPowerOffWhen.Hops; len(got) != 2 || got[0] != "edge-a" || got[1] != "edge-b" {
		t.Errorf("safeToPowerOffWhen.hops = %v, want [edge-a edge-b]", got)
	}
	if result.SafeToPowerOffWhen.Revision != before+1 {
		t.Errorf("safeToPowerOffWhen.revision = %d, want %d", result.SafeToPowerOffWhen.Revision, before+1)
	}

	draining := hopByName(t, s, "b")
	if draining.State != chain.StateDraining {
		t.Fatalf("b is %q, want draining", draining.State)
	}
	if draining.Host != "10.0.0.2" || draining.SecretHash == "" {
		t.Error("a draining hop keeps its host and its secret: it is still serving its neighbours")
	}
	if got := nextHopName(t, s, "b"); got != "a" {
		t.Errorf("a draining hop keeps dialling its own next hop, got %q, want a", got)
	}
	// The live topology runs past it.
	for _, edge := range []string{"edge-a", "edge-b"} {
		if got := nextHopName(t, s, edge); got != "a" {
			t.Errorf("%s hangs off %q, want a", edge, got)
		}
	}

	// The editor's card names who is still awaited.
	state, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Draining) != 1 || state.Draining[0].Name != "b" {
		t.Fatalf("draining cards = %+v, want one for b", state.Draining)
	}
	if len(state.Draining[0].Waiting) != 2 {
		t.Errorf("waiting = %v, want both edges", state.Draining[0].Waiting)
	}
}

// §4.5.1 step 3 — with nobody to serve, the row and the secret go at once. An
// edge has only clients outward of it, so this is how every edge leaves.
func TestChainDeleteWithoutNeighboursIsImmediate(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	edge := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	result, err := s.Delete(edge.Id, false, false)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.State != DeleteStateDeleted {
		t.Fatalf("state %q, want %q", result.State, DeleteStateDeleted)
	}
	if result.DrainUntil != 0 {
		t.Errorf("drainUntil %d, want 0 for an immediate delete", result.DrainUntil)
	}
	if len(result.SafeToPowerOffWhen.Hops) != 0 {
		t.Errorf("hops = %v, want empty: there is nobody to wait for", result.SafeToPowerOffWhen.Hops)
	}
	if hopExists(t, s, "edge-a") {
		t.Fatal("the row must be gone")
	}

	// A hop that was created and never entered has no box either.
	pending := addHop(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	result, err = s.Delete(pending.Id, false, false)
	if err != nil {
		t.Fatalf("Delete pending: %v", err)
	}
	if result.State != DeleteStateDeleted {
		t.Errorf("a pending hop leaves at once, got %q", result.State)
	}
}

// §4.5.4 — the departure ends when every former neighbour has confirmed the
// revision it started in, and §4.5.6: the revision moves a second time only
// when the departing hop's next hop was a hop, which loses an entry from its
// hops[] and would otherwise keep admitting a dead secret.
func TestChainSweepFinishesOnAcknowledgements(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	result, err := s.Delete(b.Id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	after := revisionOf(t, s)

	// Nothing has confirmed yet: the row stays.
	if err := s.SweepDraining(); err != nil {
		t.Fatalf("SweepDraining: %v", err)
	}
	if !hopExists(t, s, "b") {
		t.Fatal("the departure must wait for its neighbour")
	}

	confirmRevision(t, "edge-a", result.DrainRevision)
	if err := s.SweepDraining(); err != nil {
		t.Fatalf("SweepDraining: %v", err)
	}
	if hopExists(t, s, "b") {
		t.Fatal("with every neighbour confirmed the row must be gone")
	}
	if revisionOf(t, s) != after+1 {
		t.Errorf("b's next hop was the hop a, which loses an entry from hops[] — "+
			"the finish must bump the revision a second time, got %d want %d", revisionOf(t, s), after+1)
	}
}

// §4.5.6 — when the departing hop dialled the panel rather than a hop, no
// document changes at the finish and the revision stays where it is.
func TestChainSweepDoesNotBumpWhenNextHopIsThePanel(t *testing.T) {
	s := newChainService(t)

	a := addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	result, err := s.Delete(a.Id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != DeleteStateDraining {
		t.Fatalf("state %q, want draining: the edge hangs off a", result.State)
	}
	after := revisionOf(t, s)

	confirmRevision(t, "edge-a", result.DrainRevision)
	if err := s.SweepDraining(); err != nil {
		t.Fatal(err)
	}
	if hopExists(t, s, "a") {
		t.Fatal("the row must be gone")
	}
	if revisionOf(t, s) != after {
		t.Errorf("nothing's document changed, so the revision must stay at %d, got %d", after, revisionOf(t, s))
	}
}

// §4.5.4 — a neighbour that never confirms must not keep the row alive for
// ever: the deadline ends the departure and says who was still behind.
func TestChainSweepFinishesOnTimeout(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := s.Delete(b.Id, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepDraining(); err != nil {
		t.Fatal(err)
	}
	if !hopExists(t, s, "b") {
		t.Fatal("an unconfirmed departure before its deadline must stay")
	}

	expireDrain(t, "b")
	if err := s.SweepDraining(); err != nil {
		t.Fatal(err)
	}
	if hopExists(t, s, "b") {
		t.Fatal("past the deadline the row must go, however far behind the neighbour is")
	}
}

// §4.5.4 — a neighbour that has itself left the registry is not waited for:
// nothing will ever confirm on its behalf.
func TestChainSweepIgnoresNeighboursThatLeft(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	edge := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := s.Delete(b.Id, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(edge.Id, false, false); err != nil {
		t.Fatalf("deleting the edge: %v", err)
	}
	if err := s.SweepDraining(); err != nil {
		t.Fatal(err)
	}
	if hopExists(t, s, "b") {
		t.Fatal("with its only neighbour gone the departure has nothing left to wait for")
	}
}

// §4.5.5 and §2.7 invariant 8 — draining is terminal. Every write refuses with
// one stable code, a second del is idempotent, and skipDrain is the way out.
func TestChainDrainingRefusesEveryWrite(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	first, err := s.Delete(b.Id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	after := revisionOf(t, s)

	host := "10.0.0.99"
	wantCode(t, s.Update(b.Id, UpdateHopInput{Host: &host}), CodeHopIsDraining)
	wantCode(t, s.SetActive(b.Id), CodeHopIsDraining)
	_, _, err = s.ReissueToken(b.Id)
	wantCode(t, err, CodeHopIsDraining)

	// A name stays taken while the row lives.
	_, _, _, err = s.Add(AddHopInput{Name: "b", Host: "10.0.0.3", Role: chain.RoleInner})
	wantCode(t, err, CodeNameTaken)

	// A second del answers the same card and extends nothing.
	again, err := s.Delete(b.Id, false, false)
	if err != nil {
		t.Fatalf("a repeated del must not fail: %v", err)
	}
	if again.State != DeleteStateDraining || again.DrainRevision != first.DrainRevision ||
		again.DrainUntil != first.DrainUntil {
		t.Errorf("repeated del = %+v, want the first card %+v", again, first)
	}
	if revisionOf(t, s) != after {
		t.Errorf("a repeated del must not move the revision, got %d want %d", revisionOf(t, s), after)
	}

	// skipDrain is what an owner who needs the name back right now has.
	done, err := s.Delete(b.Id, false, true)
	if err != nil {
		t.Fatalf("del with skipDrain: %v", err)
	}
	if done.State != DeleteStateDeleted {
		t.Errorf("state %q, want deleted", done.State)
	}
	if hopExists(t, s, "b") {
		t.Fatal("skipDrain drops the row at once")
	}
}

// §4.5.9 — the runbook's delete of a hop that is already dead: skipDrain
// re-chains the neighbour and takes the row with it in one step, because a
// departure through a dead box serves nobody.
func TestChainDeleteSkipDrainIsImmediate(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	before := revisionOf(t, s)
	result, err := s.Delete(b.Id, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != DeleteStateDeleted {
		t.Fatalf("state %q, want deleted", result.State)
	}
	if hopExists(t, s, "b") {
		t.Fatal("skipDrain deletes the row there and then")
	}
	if revisionOf(t, s) != before+1 {
		t.Errorf("revision %d, want %d", revisionOf(t, s), before+1)
	}
	if got := nextHopName(t, s, "edge-a"); got != "a" {
		t.Errorf("the edge hangs off %q, want a", got)
	}
}

// §4.5.7 — the mirror case. Re-issuing the token of a hop that has entered
// leaves it in the live path: its box is alive, it is still its neighbours'
// next hop, and it stays in the documents with state pending. Dropping it out
// of the topology here would re-chain the neighbour past a living box over a
// channel that runs through that same box — the stand's freeze again.
func TestChainReissueKeepsAReEnteringHopInThePath(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, _, err := s.ReissueToken(b.Id); err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}
	reloaded := hopByName(t, s, "b")
	if reloaded.State != chain.StatePending {
		t.Fatalf("b is %q, want pending", reloaded.State)
	}
	if reloaded.SecretHash == "" {
		t.Fatal("the secret hash stays until the new box really enters — it is what tells a re-entry apart")
	}
	if got := nextHopName(t, s, "edge-a"); got != "b" {
		t.Errorf("the edge still hangs off %q, want the living b", got)
	}

	// And it is deleted through draining like any living hop (§4.5.7 end).
	result, err := s.Delete(reloaded.Id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != DeleteStateDraining {
		t.Errorf("a re-entering hop leaves through draining, got %q", result.State)
	}
}

// §4.5.8 — inserting an inner while somebody is draining puts the new hop in
// the live path, not behind the departing one.
func TestChainInsertionSkipsADrainingHop(t *testing.T) {
	s := newChainService(t)

	addJoined(t, s, AddHopInput{Name: "a", Host: "10.0.0.1", Role: chain.RoleInner})
	b := addJoined(t, s, AddHopInput{Name: "b", Host: "10.0.0.2", Role: chain.RoleInner})
	addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := s.Delete(b.Id, false, false); err != nil {
		t.Fatal(err)
	}
	m := addJoined(t, s, AddHopInput{Name: "m", Host: "10.0.0.3", Role: chain.RoleInner})
	if got := nextHopName(t, s, m.Name); got != "a" {
		t.Errorf("the new inner hangs off %q, want the live a rather than the draining b", got)
	}
	if got := nextHopName(t, s, "edge-a"); got != "m" {
		t.Errorf("the edge hangs off %q, want the new live inner m", got)
	}
	if got := nextHopName(t, s, "b"); got != "a" {
		t.Errorf("the draining hop keeps its own next hop %q, want a", got)
	}
}
