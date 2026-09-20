package service

import (
	"errors"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// newChainJoin gives a join service that builds real documents: the response
// of a join carries one, so the two halves are tested together.
func newChainJoin(t *testing.T) (*ChainJoinService, *ChainService) {
	t.Helper()
	documents, registry := newChainDocuments(t)
	return &ChainJoinService{documentService: *documents}, registry
}

// pendingHop creates a hop the owner has just added and returns it with its
// join token in the clear — the state a box finds the registry in.
func pendingHop(t *testing.T, s *ChainService, in AddHopInput) (*model.ChainHop, string) {
	t.Helper()
	hop, token, _, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	return hop, token
}

func TestChainJoinEntersTheHop(t *testing.T) {
	join, registry := newChainJoin(t)
	hop, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	before := revisionOf(t, registry)

	response, err := join.Join(JoinRequest{Token: token, ObservedAddr: "198.51.100.44"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if response.HopId != hop.Id || response.Name != "edge-a" {
		t.Fatalf("Join returned hop %d %q, want %d %q", response.HopId, response.Name, hop.Id, "edge-a")
	}
	if len(response.Secret) != chain.SecretLength {
		t.Fatalf("hop secret %q is not %d characters", response.Secret, chain.SecretLength)
	}
	if response.PollSeconds <= 0 {
		t.Fatalf("pollSeconds is %d", response.PollSeconds)
	}
	if response.Document == nil || response.Document.Self.Name != "edge-a" {
		t.Fatalf("the response carries no document for the hop: %+v", response.Document)
	}

	stored := hopByName(t, registry, "edge-a")
	if stored.State != chain.StateJoined {
		t.Fatalf("hop state is %q, want %q", stored.State, chain.StateJoined)
	}
	if stored.SecretHash != chain.HashSecret(response.Secret) {
		t.Fatal("the stored secret hash is not the hash of the secret handed to the box")
	}
	if stored.JoinTokenHash != "" || stored.JoinTokenExpires != 0 {
		t.Fatal("the join token survived the join")
	}
	if stored.JoinedAt == 0 || stored.ObservedAddr != "198.51.100.44" {
		t.Fatalf("joinedAt %d, observedAddr %q", stored.JoinedAt, stored.ObservedAddr)
	}
	if after := revisionOf(t, registry); after != before+1 {
		t.Fatalf("revision %d → %d, want one bump", before, after)
	}
}

func TestChainJoinTakesTheHostTheBoxReports(t *testing.T) {
	join, registry := newChainJoin(t)
	_, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "old.example.net", Role: chain.RoleEdge})

	if _, err := join.Join(JoinRequest{Token: token, Host: "new.example.net", SubPort: 8443, SubScheme: "http"}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	stored := hopByName(t, registry, "edge-a")
	if stored.Host != "new.example.net" || stored.SubPort != 8443 || stored.SubScheme != "http" {
		t.Fatalf("the hop kept %s:%d %s", stored.Host, stored.SubPort, stored.SubScheme)
	}
}

func TestChainJoinKeepsTheRegistryHostWhenTheBoxSendsNone(t *testing.T) {
	join, registry := newChainJoin(t)
	_, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := join.Join(JoinRequest{Token: token}); err != nil {
		t.Fatalf("Join: %v", err)
	}
	stored := hopByName(t, registry, "edge-a")
	if stored.Host != "a.example.net" || stored.SubPort != defaultHopSubPort {
		t.Fatalf("the hop kept %s:%d", stored.Host, stored.SubPort)
	}
}

func TestChainJoinRejectsAnUnknownToken(t *testing.T) {
	join, registry := newChainJoin(t)
	pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	before := revisionOf(t, registry)

	_, err := join.Join(JoinRequest{Token: chain.NewSecret()})
	if !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("Join with an unknown token: %v, want ErrJoinRejected", err)
	}
	if hopByName(t, registry, "edge-a").State != chain.StatePending {
		t.Fatal("a refused join still moved the hop")
	}
	if after := revisionOf(t, registry); after != before {
		t.Fatalf("a refused join moved the revision %d → %d", before, after)
	}
}

func TestChainJoinRejectsAMalformedToken(t *testing.T) {
	join, registry := newChainJoin(t)
	pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	for _, token := range []string{"", "   ", "short"} {
		if _, err := join.Join(JoinRequest{Token: token}); !errors.Is(err, ErrJoinRejected) {
			t.Fatalf("Join with token %q: %v, want ErrJoinRejected", token, err)
		}
	}
}

func TestChainJoinRejectsAnExpiredToken(t *testing.T) {
	join, registry := newChainJoin(t)
	hop, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	expired := time.Now().Add(-time.Minute).UnixMilli()
	if err := database.GetDB().Model(&model.ChainHop{}).Where("id = ?", hop.Id).
		Update("join_token_expires", expired).Error; err != nil {
		t.Fatalf("expire the token: %v", err)
	}

	if _, err := join.Join(JoinRequest{Token: token}); !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("Join with an expired token: %v, want ErrJoinRejected", err)
	}
	stored := hopByName(t, registry, "edge-a")
	if stored.State != chain.StatePending || stored.JoinTokenHash == "" {
		t.Fatal("an expired join spent the token")
	}
}

func TestChainJoinRejectsASpentToken(t *testing.T) {
	join, registry := newChainJoin(t)
	_, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	first, err := join.Join(JoinRequest{Token: token})
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	before := revisionOf(t, registry)

	if _, err := join.Join(JoinRequest{Token: token}); !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("second Join with the same token: %v, want ErrJoinRejected", err)
	}
	stored := hopByName(t, registry, "edge-a")
	if stored.SecretHash != chain.HashSecret(first.Secret) {
		t.Fatal("the refused second join replaced the hop secret")
	}
	if after := revisionOf(t, registry); after != before {
		t.Fatalf("the refused second join moved the revision %d → %d", before, after)
	}
}

func TestChainJoinRejectsAHopThatIsAlreadyJoined(t *testing.T) {
	join, registry := newChainJoin(t)
	hop, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	// A hop that entered but whose token hash somehow survived: the state, not
	// the token, is what decides.
	if err := database.GetDB().Model(&model.ChainHop{}).Where("id = ?", hop.Id).
		Update("state", chain.StateJoined).Error; err != nil {
		t.Fatalf("mark joined: %v", err)
	}

	if _, err := join.Join(JoinRequest{Token: token}); !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("Join of a joined hop: %v, want ErrJoinRejected", err)
	}
}

func TestChainJoinReJoinAfterReissue(t *testing.T) {
	join, registry := newChainJoin(t)
	_, token := pendingHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	first, err := join.Join(JoinRequest{Token: token})
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	reissued, _, err := registry.ReissueToken(first.HopId)
	if err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}

	second, err := join.Join(JoinRequest{Token: reissued})
	if err != nil {
		t.Fatalf("Join after ReissueToken: %v", err)
	}
	if second.Secret == first.Secret {
		t.Fatal("the re-entered box got the old hop secret back")
	}
	stored := hopByName(t, registry, "edge-a")
	if stored.SecretHash != chain.HashSecret(second.Secret) || stored.State != chain.StateJoined {
		t.Fatalf("after the second join the hop is %q with a stale secret hash", stored.State)
	}
}
