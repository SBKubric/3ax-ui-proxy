package service

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// The neighbour target of an edge and the chain-following inbounds that take
// it over on a switch (ticket #139, ADR 0005).

// followStream is a Reality inbound's stream settings the way the inbound
// form writes them: "target", a list of cover names, and the client-side
// settings block the panel strips before xray sees it.
const followStream = `{"network":"xhttp","security":"reality","realitySettings":{` +
	`"show":false,"xver":0,"target":"www.original.example:443",` +
	`"serverNames":["www.original.example","original.example"],` +
	`"privateKey":"priv","shortIds":["ab12"],` +
	`"settings":{"publicKey":"pub","fingerprint":"chrome","serverName":"","spiderX":"/"}},` +
	`"xhttpSettings":{"path":"/x"}}`

// seedInbound stores an inbound straight in the database: the fixture of a
// registry test, not the inbound service's own validation.
func seedInbound(t *testing.T, port int, follow bool, stream string) *model.Inbound {
	t.Helper()
	inbound := &model.Inbound{
		UserId:         1,
		Remark:         "reality-" + strconv.Itoa(port),
		Enable:         true,
		Port:           port,
		Protocol:       model.VLESS,
		Tag:            "inbound-" + strconv.Itoa(port),
		Settings:       `{"clients":[],"decryption":"none"}`,
		StreamSettings: stream,
		Sniffing:       `{"enabled":false}`,
		FollowChain:    follow,
	}
	if err := database.GetDB().Create(inbound).Error; err != nil {
		t.Fatalf("create inbound %d: %v", port, err)
	}
	return inbound
}

// realityOf reads an inbound back through the inbound service and returns its
// realitySettings.
func realityOf(t *testing.T, id int) map[string]any {
	t.Helper()
	inbound, err := (&InboundService{}).GetInbound(id)
	if err != nil {
		t.Fatalf("GetInbound(%d): %v", id, err)
	}
	var stream map[string]any
	if err := json.Unmarshal([]byte(inbound.StreamSettings), &stream); err != nil {
		t.Fatalf("stream settings of %d are not JSON: %v", id, err)
	}
	reality, _ := stream["realitySettings"].(map[string]any)
	if reality == nil {
		t.Fatalf("inbound %d has no realitySettings: %s", id, inbound.StreamSettings)
	}
	return reality
}

func wantReality(t *testing.T, id int, target string, serverNames ...string) {
	t.Helper()
	reality := realityOf(t, id)
	if got := reality["target"]; got != target {
		t.Errorf("inbound %d: target %v, want %q", id, got, target)
	}
	names := []string{}
	if list, ok := reality["serverNames"].([]any); ok {
		for _, name := range list {
			names = append(names, name.(string))
		}
	}
	if !reflect.DeepEqual(names, serverNames) {
		t.Errorf("inbound %d: serverNames %v, want %v", id, names, serverNames)
	}
}

// Two joined edges, each with its own neighbour target: edge-a gives only the
// target (its server name is the target's host), edge-b a target by address
// and the name the site answers to.
func neighbourChain(t *testing.T) (*ChainService, *model.ChainHop, *model.ChainHop) {
	t.Helper()
	s := newChainService(t)
	a := addJoined(t, s, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge,
		RealityTarget: "www.neighbour-a.example:443"})
	b := addJoined(t, s, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge,
		RealityTarget: "198.51.100.20:443", RealityServerName: "www.neighbour-b.example"})
	return s, a, b
}

func TestSetActiveRewritesChainFollowingInbounds(t *testing.T) {
	s, a, b := neighbourChain(t)
	followed := seedInbound(t, 443, true, followStream)
	plain := seedInbound(t, 8443, false, followStream)

	if err := s.SetActive(a.Id); err != nil {
		t.Fatalf("SetActive(edge-a): %v", err)
	}
	wantReality(t, followed.Id, "www.neighbour-a.example:443", "www.neighbour-a.example")
	wantReality(t, plain.Id, "www.original.example:443", "www.original.example", "original.example")

	if err := s.SetActive(b.Id); err != nil {
		t.Fatalf("SetActive(edge-b): %v", err)
	}
	wantReality(t, followed.Id, "198.51.100.20:443", "www.neighbour-b.example")
	wantReality(t, plain.Id, "www.original.example:443", "www.original.example", "original.example")
}

// An edge without a neighbour target cannot take over while inbounds follow
// the chain — they would go on imitating the previous edge's neighbour — and
// nothing moves: not the active edge, not the inbounds, not the revision.
func TestSetActiveRefusesAnEdgeWithoutANeighbourTarget(t *testing.T) {
	s, a, _ := neighbourChain(t)
	bare := addJoined(t, s, AddHopInput{Name: "edge-c", Host: "c.example.net", Role: chain.RoleEdge})
	followed := seedInbound(t, 443, true, followStream)
	if err := s.SetActive(a.Id); err != nil {
		t.Fatalf("SetActive(edge-a): %v", err)
	}
	before := revisionOf(t, s)

	wantCode(t, s.SetActive(bare.Id), CodeNoNeighbourTarget)
	if active := hopByName(t, s, "edge-a"); !active.IsActive {
		t.Fatal("edge-a lost the override to a refused switch")
	}
	wantReality(t, followed.Id, "www.neighbour-a.example:443", "www.neighbour-a.example")
	if after := revisionOf(t, s); after != before {
		t.Fatalf("revision moved from %d to %d on a refused switch", before, after)
	}
}

// Without a single chain-following inbound the neighbour target is nobody's
// business: switching to a bare edge is what it always was.
func TestSetActiveWithoutFollowersNeedsNoNeighbourTarget(t *testing.T) {
	s := newChainService(t)
	bare := addJoined(t, s, AddHopInput{Name: "edge-c", Host: "c.example.net", Role: chain.RoleEdge})
	plain := seedInbound(t, 8443, false, followStream)
	if err := s.SetActive(bare.Id); err != nil {
		t.Fatalf("SetActive(edge-c) with no followers: %v", err)
	}
	wantReality(t, plain.Id, "www.original.example:443", "www.original.example", "original.example")
}

// /proxy off leaves the followers exactly where the last active edge put
// them (owner decision on #139): their links keep working, only the host
// changes to the real server's.
func TestClearActiveLeavesTheFollowersAlone(t *testing.T) {
	s, a, _ := neighbourChain(t)
	followed := seedInbound(t, 443, true, followStream)
	if err := s.SetActive(a.Id); err != nil {
		t.Fatalf("SetActive(edge-a): %v", err)
	}
	if err := s.ClearActive(); err != nil {
		t.Fatalf("ClearActive: %v", err)
	}
	wantReality(t, followed.Id, "www.neighbour-a.example:443", "www.neighbour-a.example")
}

func strRef(value string) *string { return &value }

// The orchestrator writes the neighbour target of an edge that is already
// active: the followers take it at once, and the revision moves so the boxes
// fetch a document carrying it.
func TestUpdateOfTheActiveEdgesTargetRewritesTheFollowers(t *testing.T) {
	s, a, b := neighbourChain(t)
	followed := seedInbound(t, 443, true, followStream)
	if err := s.SetActive(a.Id); err != nil {
		t.Fatalf("SetActive(edge-a): %v", err)
	}
	before := revisionOf(t, s)

	if err := s.Update(a.Id, UpdateHopInput{RealityTarget: strRef("www.moved-a.example:8443")}); err != nil {
		t.Fatalf("Update(edge-a target): %v", err)
	}
	wantReality(t, followed.Id, "www.moved-a.example:8443", "www.moved-a.example")
	if after := revisionOf(t, s); after != before+1 {
		t.Fatalf("revision %d after a neighbour target change, want %d", after, before+1)
	}

	if err := s.Update(a.Id, UpdateHopInput{RealityServerName: strRef("cdn.moved-a.example")}); err != nil {
		t.Fatalf("Update(edge-a server name): %v", err)
	}
	wantReality(t, followed.Id, "www.moved-a.example:8443", "cdn.moved-a.example")

	// A standby edge's neighbour is only stored: the followers imitate the
	// active edge's, not whichever was edited last.
	if err := s.Update(b.Id, UpdateHopInput{RealityTarget: strRef("198.51.100.21:443")}); err != nil {
		t.Fatalf("Update(edge-b target): %v", err)
	}
	wantReality(t, followed.Id, "www.moved-a.example:8443", "cdn.moved-a.example")
	if got := hopByName(t, s, "edge-b"); got.RealityTarget != "198.51.100.21:443" || got.RealityServerName != "www.neighbour-b.example" {
		t.Fatalf("edge-b neighbour is %q/%q after the update", got.RealityTarget, got.RealityServerName)
	}

	// Taking the active edge's neighbour away would leave the followers with
	// nothing to imitate.
	wantCode(t, s.Update(a.Id, UpdateHopInput{RealityTarget: strRef("")}), CodeNoNeighbourTarget)
	wantReality(t, followed.Id, "www.moved-a.example:8443", "cdn.moved-a.example")
	// A standby's may go: nothing follows it yet.
	if err := s.Update(b.Id, UpdateHopInput{RealityTarget: strRef("")}); err != nil {
		t.Fatalf("clearing the standby's neighbour target: %v", err)
	}
	if got := hopByName(t, s, "edge-b"); got.RealityTarget != "" || got.RealityServerName != "" {
		t.Fatalf("edge-b neighbour is %q/%q after clearing it", got.RealityTarget, got.RealityServerName)
	}
}

func TestNeighbourTargetValidation(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		target     string
		serverName string
		code       string
	}{
		{name: "an inner has no neighbour", role: chain.RoleInner, target: "www.n.example:443", code: CodeRealityTargetEdgeOnly},
		{name: "no port", role: chain.RoleEdge, target: "www.n.example", code: CodeInvalidRealityTarget},
		{name: "port out of range", role: chain.RoleEdge, target: "www.n.example:70000", code: CodeInvalidRealityTarget},
		{name: "port not a number", role: chain.RoleEdge, target: "www.n.example:https", code: CodeInvalidRealityTarget},
		{name: "no host", role: chain.RoleEdge, target: ":443", code: CodeInvalidRealityTarget},
		{name: "an address needs a name", role: chain.RoleEdge, target: "198.51.100.20:443", code: CodeInvalidRealityServerName},
		{name: "a name is not an address", role: chain.RoleEdge, target: "198.51.100.20:443", serverName: "198.51.100.20", code: CodeInvalidRealityServerName},
		{name: "a name has no port", role: chain.RoleEdge, target: "www.n.example:443", serverName: "www.n.example:443", code: CodeInvalidRealityServerName},
		{name: "a name without a target", role: chain.RoleEdge, serverName: "www.n.example", code: CodeInvalidRealityTarget},
		{name: "an IPv6 target with a name", role: chain.RoleEdge, target: "[2001:db8::1]:443", serverName: "www.n.example", code: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newChainService(t)
			_, _, _, err := s.Add(AddHopInput{Name: "hop", Host: "h.example.net", Role: tc.role,
				RealityTarget: tc.target, RealityServerName: tc.serverName})
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Add: %v", err)
				}
				return
			}
			wantCode(t, err, tc.code)
		})
	}

	// Update refuses an inner the same way Add does.
	s := newChainService(t)
	inner := addHop(t, s, AddHopInput{Name: "core-1", Host: "10.0.0.7", Role: chain.RoleInner})
	wantCode(t, s.Update(inner.Id, UpdateHopInput{RealityTarget: strRef("www.n.example:443")}), CodeRealityTargetEdgeOnly)
}

// followerInput is a chain-following inbound as the inbound form submits it.
func followerInput(port int, stream string) *model.Inbound {
	return &model.Inbound{
		UserId: 1, Remark: "follower", Enable: false, Port: port,
		Protocol: model.VLESS, Tag: "inbound-" + strconv.Itoa(port),
		Settings:       `{"clients":[],"decryption":"none"}`,
		StreamSettings: stream, Sniffing: `{"enabled":false}`,
		FollowChain: true,
	}
}

// PrepareFollower is what the inbound service runs on every save: the flag is
// refused on anything but Reality, and an inbound flagged while an edge is
// active takes that edge's neighbour right away instead of at the next
// switch.
func TestPrepareFollower(t *testing.T) {
	tlsStream := `{"network":"tcp","security":"tls","tlsSettings":{"serverName":"x.example"}}`

	t.Run("the flag needs Reality", func(t *testing.T) {
		s := newChainService(t)
		wantCode(t, s.PrepareFollower(followerInput(443, tlsStream)), CodeFollowChainNotReality)
		wantCode(t, s.PrepareFollower(followerInput(443, "")), CodeFollowChainNotReality)
	})

	t.Run("an unflagged inbound is nobody's business", func(t *testing.T) {
		s, a, _ := neighbourChain(t)
		if err := s.SetActive(a.Id); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		inbound := followerInput(443, tlsStream)
		inbound.FollowChain = false
		if err := s.PrepareFollower(inbound); err != nil {
			t.Fatalf("PrepareFollower(unflagged): %v", err)
		}
		if inbound.StreamSettings != tlsStream {
			t.Fatalf("an unflagged inbound was rewritten: %s", inbound.StreamSettings)
		}
	})

	t.Run("no active edge keeps the inbound's own cover", func(t *testing.T) {
		s := newChainService(t)
		inbound := followerInput(443, followStream)
		if err := s.PrepareFollower(inbound); err != nil {
			t.Fatalf("PrepareFollower: %v", err)
		}
		if inbound.StreamSettings != followStream {
			t.Fatalf("rewritten with no active edge: %s", inbound.StreamSettings)
		}
	})

	t.Run("an active edge with a neighbour rewrites it at once", func(t *testing.T) {
		s, _, b := neighbourChain(t)
		if err := s.SetActive(b.Id); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		saved, _, err := (&InboundService{}).AddInbound(followerInput(24443, followStream))
		if err != nil {
			t.Fatalf("AddInbound: %v", err)
		}
		wantReality(t, saved.Id, "198.51.100.20:443", "www.neighbour-b.example")
		if !saved.FollowChain {
			t.Fatal("the flag was not stored")
		}
	})

	t.Run("an active edge without a neighbour refuses the flag", func(t *testing.T) {
		s := newChainService(t)
		bare := addJoined(t, s, AddHopInput{Name: "edge-c", Host: "c.example.net", Role: chain.RoleEdge})
		if err := s.SetActive(bare.Id); err != nil {
			t.Fatalf("SetActive: %v", err)
		}
		_, _, err := (&InboundService{}).AddInbound(followerInput(24443, followStream))
		wantCode(t, err, CodeNoNeighbourTarget)
	})
}
