package service

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

func newChainDocuments(t *testing.T) (*ChainDocumentService, *ChainService) {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	// One ordinary inbound: the port composition is not what these tests are
	// about, but a document with no port at all says little.
	if err := database.GetDB().Create(&model.Inbound{
		UserId: 1, Enable: true, Listen: "0.0.0.0", Port: 443,
		Protocol: model.VLESS, Tag: "inbound-443", Remark: "vless", Settings: `{"clients":[]}`,
	}).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	documents := &ChainDocumentService{}
	if err := documents.settingService.SetChainPanelHost("198.51.100.1"); err != nil {
		t.Fatalf("SetChainPanelHost: %v", err)
	}
	return documents, &ChainService{}
}

// enteredHop creates a hop and walks it through the join, which is how every hop
// that appears in a document got there.
func enteredHop(t *testing.T, s *ChainService, in AddHopInput) *model.ChainHop {
	t.Helper()
	hop, _, _, err := s.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	if err := s.MarkJoined(hop.Id, chain.HashSecret("secret-"+in.Name), ""); err != nil {
		t.Fatalf("MarkJoined(%s): %v", in.Name, err)
	}
	return hop
}

func hopNames(hops []chain.Hop) []string {
	names := make([]string, 0, len(hops))
	for _, hop := range hops {
		names = append(names, hop.Name)
	}
	return names
}

func sameNames(got []chain.Hop, want ...string) bool {
	names := hopNames(got)
	if len(names) != len(want) {
		return false
	}
	for index := range names {
		if names[index] != want[index] {
			return false
		}
	}
	return true
}

// exampleChain is the chain of §3.2: real ← inner-1 ← inner-2 ← {edge-a
// (active), edge-b}.
func exampleChain(t *testing.T) (*ChainDocumentService, *ChainService) {
	t.Helper()
	documents, registry := newChainDocuments(t)
	enteredHop(t, registry, AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	enteredHop(t, registry, AddHopInput{Name: "inner-2", Host: "203.0.113.9", Role: chain.RoleInner})
	edgeA := enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	enteredHop(t, registry, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	if err := registry.SetActive(edgeA.Id); err != nil {
		t.Fatalf("SetActive(edge-a): %v", err)
	}
	return documents, registry
}

// TestTheThreeViewpointsOfTheExample walks §3.2 from each end: every hop sees
// itself and what is outward of it, one address inward, and the same ports.
func TestTheThreeViewpointsOfTheExample(t *testing.T) {
	documents, _ := exampleChain(t)

	inner1, err := documents.Build("inner-1")
	if err != nil {
		t.Fatalf("Build(inner-1): %v", err)
	}
	if inner1.Version != chain.DocumentVersion {
		t.Errorf("version = %d, want %d", inner1.Version, chain.DocumentVersion)
	}
	if inner1.Self != (chain.Self{Name: "inner-1", Role: chain.RoleInner, Host: "10.0.0.7", State: chain.StateJoined}) {
		t.Errorf("inner-1 self = %+v", inner1.Self)
	}
	if inner1.NextHop.Host != "198.51.100.1" {
		t.Errorf("the innermost hop polls %q, want the panel", inner1.NextHop.Host)
	}
	if !sameNames(inner1.Hops, "inner-1", "inner-2", "edge-a", "edge-b") {
		t.Errorf("inner-1 hops = %v, want the whole chain outward", hopNames(inner1.Hops))
	}
	if inner1.ActiveEdge != "edge-a" {
		t.Errorf("inner-1 activeEdge = %q, want edge-a", inner1.ActiveEdge)
	}
	if len(inner1.Ports) != 1 || inner1.Ports[0].Port != 443 {
		t.Errorf("inner-1 ports = %+v, want the panel's 443", inner1.Ports)
	}
	if inner1.GeneratedAt == 0 {
		t.Error("generatedAt is zero")
	}

	inner2, err := documents.Build("inner-2")
	if err != nil {
		t.Fatalf("Build(inner-2): %v", err)
	}
	if inner2.NextHop.Host != "10.0.0.7" {
		t.Errorf("inner-2 polls %q, want inner-1", inner2.NextHop.Host)
	}
	if !sameNames(inner2.Hops, "inner-2", "edge-a", "edge-b") {
		t.Errorf("inner-2 hops = %v, want inner-1 truncated away", hopNames(inner2.Hops))
	}
	if inner2.ActiveEdge != "edge-a" {
		t.Errorf("inner-2 activeEdge = %q, want edge-a", inner2.ActiveEdge)
	}

	edgeA, err := documents.Build("edge-a")
	if err != nil {
		t.Fatalf("Build(edge-a): %v", err)
	}
	if edgeA.NextHop.Host != "203.0.113.9" {
		t.Errorf("edge-a polls %q, want inner-2", edgeA.NextHop.Host)
	}
	if !sameNames(edgeA.Hops, "edge-a") {
		t.Errorf("edge-a hops = %v, want itself only", hopNames(edgeA.Hops))
	}
	if edgeA.ActiveEdge != "edge-a" {
		t.Errorf("the active edge does not know it is active: %q", edgeA.ActiveEdge)
	}
	if edgeA.Hops[0].SecretHash != chain.HashSecret("secret-edge-a") {
		t.Error("the hop's own secret hash is missing from its document")
	}
}

// TestAStandbyEdgeLearnsNothingAboutTheActiveOne: the edge beside a hop is
// sideways, not outward. A seized standby must not learn its neighbour's name,
// and must not learn that it is the active one either (§3.2).
func TestAStandbyEdgeLearnsNothingAboutTheActiveOne(t *testing.T) {
	documents, _ := exampleChain(t)

	edgeB, err := documents.Build("edge-b")
	if err != nil {
		t.Fatalf("Build(edge-b): %v", err)
	}
	if !sameNames(edgeB.Hops, "edge-b") {
		t.Errorf("edge-b hops = %v, want itself only", hopNames(edgeB.Hops))
	}
	if edgeB.ActiveEdge != "" {
		t.Errorf("a standby edge was told the active edge is %q", edgeB.ActiveEdge)
	}
}

// TestAPendingHopIsInvisible: the owner may create a hop long before its box
// exists (§2.6.3). Until it joins it is in no document at all — not as one of
// its own, not in a hops list.
func TestAPendingHopIsInvisible(t *testing.T) {
	documents, registry := newChainDocuments(t)
	enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if _, _, _, err := registry.Add(AddHopInput{Name: "later", Host: "10.0.0.9", Role: chain.RoleInner}); err != nil {
		t.Fatalf("Add(later): %v", err)
	}

	all, err := documents.BuildAll()
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if _, found := all["later"]; found {
		t.Error("a pending hop got a document of its own")
	}
	edgeA := all["edge-a"]
	if edgeA == nil {
		t.Fatal("edge-a has no document")
	}
	for _, hop := range edgeA.Hops {
		if hop.Name == "later" {
			t.Error("a pending hop appears in a hops list")
		}
	}
}

// TestAPendingInnerIsNotANextHop: the registry re-chains next_hop_id onto a
// new inner at once, pending or not. A document must resolve past it, or the
// outer hop would be told to poll a box that does not exist yet and the chain
// would go dark until someone installed it.
func TestAPendingInnerIsNotANextHop(t *testing.T) {
	documents, registry := newChainDocuments(t)
	enteredHop(t, registry, AddHopInput{Name: "a", Host: "10.0.0.7", Role: chain.RoleInner})
	middle, _, _, err := registry.Add(AddHopInput{Name: "m", Host: "10.0.0.8", Role: chain.RoleInner})
	if err != nil {
		t.Fatalf("Add(m): %v", err)
	}
	enteredHop(t, registry, AddHopInput{Name: "b", Host: "10.0.0.9", Role: chain.RoleInner})

	document, err := documents.Build("b")
	if err != nil {
		t.Fatalf("Build(b): %v", err)
	}
	if document.NextHop.Host != "10.0.0.7" {
		t.Errorf("b polls %q, want a — the pending hop between them is not there yet", document.NextHop.Host)
	}
	for _, hop := range document.Hops {
		if hop.Name == "m" {
			t.Error("a pending inner appears in the hops list")
		}
	}

	if err := registry.MarkJoined(middle.Id, chain.HashSecret("secret-m"), ""); err != nil {
		t.Fatalf("MarkJoined(m): %v", err)
	}
	document, err = documents.Build("b")
	if err != nil {
		t.Fatalf("Build(b) after the join: %v", err)
	}
	if document.NextHop.Host != "10.0.0.8" {
		t.Errorf("b polls %q after m joined, want m", document.NextHop.Host)
	}
}

// TestAChainOfOne: a single edge hanging off the panel is the smallest chain
// there is, and the one the runbook starts from.
func TestAChainOfOne(t *testing.T) {
	documents, registry := newChainDocuments(t)
	edge := enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err := registry.SetActive(edge.Id); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	document, err := documents.Build("edge-a")
	if err != nil {
		t.Fatalf("Build(edge-a): %v", err)
	}
	if document.NextHop.Host != "198.51.100.1" {
		t.Errorf("the only hop polls %q, want the panel", document.NextHop.Host)
	}
	if document.NextHop.SubPath == "" || document.NextHop.JsonPath == "" {
		t.Errorf("nextHop carries no subscription paths: %+v", document.NextHop)
	}
	if !sameNames(document.Hops, "edge-a") {
		t.Errorf("hops = %v, want edge-a alone", hopNames(document.Hops))
	}
}

// TestBuildRefusesAnUnknownHop: the wave serves a document by the caller's
// identity, so a name that is not in the chain is a refusal with a code, not
// an empty document.
func TestBuildRefusesAnUnknownHop(t *testing.T) {
	documents, registry := newChainDocuments(t)
	enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := documents.Build("nope"); ChainErrorCode(err) != CodeUnknownHop {
		t.Fatalf("Build(nope) error = %v, want %s", err, CodeUnknownHop)
	}
}

// TestThePanelHostSettingOverridesTheCallersView: the owner's answer wins.
// A front reaching the panel through something that rewrites Host would
// otherwise hand the chain an address no other hop can dial.
func TestThePanelHostSettingOverridesTheCallersView(t *testing.T) {
	documents, registry := newChainDocuments(t)
	enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	document, err := documents.BuildWithPanelHost("edge-a", "10.0.0.250")
	if err != nil {
		t.Fatalf("BuildWithPanelHost: %v", err)
	}
	if document.NextHop.Host != "198.51.100.1" {
		t.Errorf("the hop polls %q, want the stated chainPanelHost", document.NextHop.Host)
	}
}

// TestTheCallersViewStandsInForAnUnsetPanelHost: with no setting, the address
// the first-tier hop just reached the panel at is one that demonstrably works,
// so the owner does not have to type anything for an ordinary chain (#81
// passes the request's Host).
func TestTheCallersViewStandsInForAnUnsetPanelHost(t *testing.T) {
	documents, registry := newChainDocuments(t)
	if err := documents.settingService.SetChainPanelHost(""); err != nil {
		t.Fatalf("SetChainPanelHost: %v", err)
	}
	enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	document, err := documents.BuildWithPanelHost("edge-a", "panel.example.net")
	if err != nil {
		t.Fatalf("BuildWithPanelHost: %v", err)
	}
	if document.NextHop.Host != "panel.example.net" {
		t.Errorf("the hop polls %q, want the address it reached the panel at", document.NextHop.Host)
	}
}

// TestBuildRefusesWithoutThePanelHost: with neither a setting nor a caller to
// ask, an empty nextHop.host would send the innermost hop nowhere, and it
// would look like a working document.
func TestBuildRefusesWithoutThePanelHost(t *testing.T) {
	documents, registry := newChainDocuments(t)
	if err := documents.settingService.SetChainPanelHost(""); err != nil {
		t.Fatalf("SetChainPanelHost: %v", err)
	}
	enteredHop(t, registry, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	if _, err := documents.BuildAll(); ChainErrorCode(err) != CodePanelHostUnset {
		t.Fatalf("BuildAll error = %v, want %s", err, CodePanelHostUnset)
	}
}

// TestAnEmptyRegistryBuildsNothing, and says so without complaining about the
// panel host: a panel with no chain has no reason to have set one.
func TestAnEmptyRegistryBuildsNothing(t *testing.T) {
	documents, _ := newChainDocuments(t)
	if err := documents.settingService.SetChainPanelHost(""); err != nil {
		t.Fatalf("SetChainPanelHost: %v", err)
	}
	all, err := documents.BuildAll()
	if err != nil {
		t.Fatalf("BuildAll: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("BuildAll on an empty registry = %v", all)
	}
}

func TestETagIsTheQuotedRevision(t *testing.T) {
	if got := ETag(42); got != `"42"` {
		t.Errorf("ETag(42) = %s, want %q", got, `"42"`)
	}
}

// §4.5.2 — the documents while a hop drains. The example is the spec's:
// real ← inner-1 ← inner-2 ← {edge-a, edge-b}, with inner-2 deleted.
//
//   - inner-2 keeps a document of its own, with self.state draining, its old
//     next hop and its former neighbours (and everything outward of them) in
//     hops[]: truncating it down to its direct neighbours would strip them of
//     the hashes they need to admit their own outer neighbours.
//   - inner-1, its next hop, keeps it in hops[] with state draining — the one
//     place its secret hash still lives, which is what keeps it authenticable.
//   - the neighbours' documents do not mention it at all and point past it.
func TestADrainingHopKeepsItsDocumentAndLeavesTheLivePath(t *testing.T) {
	documents, registry := exampleChain(t)

	inner2 := hopByName(t, registry, "inner-2")
	if _, err := registry.Delete(inner2.Id, false, false); err != nil {
		t.Fatalf("Delete(inner-2): %v", err)
	}

	own, err := documents.Build("inner-2")
	if err != nil {
		t.Fatalf("Build(inner-2): %v", err)
	}
	if own.Self.State != chain.StateDraining {
		t.Errorf("inner-2 self.state = %q, want draining", own.Self.State)
	}
	if own.NextHop.Host != "10.0.0.7" {
		t.Errorf("a draining hop keeps dialling %q, want inner-1", own.NextHop.Host)
	}
	if !sameNames(own.Hops, "inner-2", "edge-a", "edge-b") {
		t.Errorf("inner-2 hops = %v, want itself and everything that was outward", hopNames(own.Hops))
	}

	inner1, err := documents.Build("inner-1")
	if err != nil {
		t.Fatalf("Build(inner-1): %v", err)
	}
	if !sameNames(inner1.Hops, "inner-1", "inner-2", "edge-a", "edge-b") {
		t.Errorf("inner-1 hops = %v, want the draining hop still listed", hopNames(inner1.Hops))
	}
	for _, listed := range inner1.Hops {
		if listed.Name != "inner-2" {
			continue
		}
		if listed.State != chain.StateDraining {
			t.Errorf("inner-1 lists inner-2 as %q, want draining", listed.State)
		}
		if listed.SecretHash == "" {
			t.Error("the draining hop's secret hash must survive in its next hop's document")
		}
	}

	for _, name := range []string{"edge-a", "edge-b"} {
		edge, err := documents.Build(name)
		if err != nil {
			t.Fatalf("Build(%s): %v", name, err)
		}
		if edge.NextHop.Host != "10.0.0.7" {
			t.Errorf("%s polls %q, want the re-chained inner-1", name, edge.NextHop.Host)
		}
		if !sameNames(edge.Hops, name) {
			t.Errorf("%s hops = %v, want itself only", name, hopNames(edge.Hops))
		}
	}
}

// §4.5.7 — a hop that is re-entering (pending with a secret hash) stays in the
// documents and stays its neighbours' next hop, unlike a hop that has never
// entered. Otherwise the panel would re-chain a neighbour past a living box
// over a channel that runs through that very box.
func TestAReEnteringHopStaysVisible(t *testing.T) {
	documents, registry := exampleChain(t)

	inner2 := hopByName(t, registry, "inner-2")
	if _, _, err := registry.ReissueToken(inner2.Id); err != nil {
		t.Fatalf("ReissueToken(inner-2): %v", err)
	}

	edge, err := documents.Build("edge-a")
	if err != nil {
		t.Fatalf("Build(edge-a): %v", err)
	}
	if edge.NextHop.Host != "203.0.113.9" {
		t.Errorf("edge-a polls %q, want the still-living inner-2", edge.NextHop.Host)
	}

	inner1, err := documents.Build("inner-1")
	if err != nil {
		t.Fatalf("Build(inner-1): %v", err)
	}
	if !sameNames(inner1.Hops, "inner-1", "inner-2", "edge-a", "edge-b") {
		t.Errorf("inner-1 hops = %v, want the re-entering hop still listed", hopNames(inner1.Hops))
	}
	for _, listed := range inner1.Hops {
		if listed.Name == "inner-2" && listed.State != chain.StatePending {
			t.Errorf("inner-1 lists inner-2 as %q, want pending", listed.State)
		}
	}
}
