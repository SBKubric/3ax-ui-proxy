package service

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

func newPanel(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
}

func chainRevision(t *testing.T) int64 {
	t.Helper()
	var setting SettingService
	revision, err := setting.GetChainRevision()
	if err != nil {
		t.Fatalf("GetChainRevision: %v", err)
	}
	return revision
}

// TestAddingAnInboundMovesTheChainRevision: the ports of the document come
// from the panel's own state (§3.8), so a new inbound is a new document — and
// a front that never hears about it never opens the port.
func TestAddingAnInboundMovesTheChainRevision(t *testing.T) {
	newPanel(t)
	registry := &ChainService{}
	hop, _, _, err := registry.Add(AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := registry.MarkJoined(hop.Id, chain.HashSecret("secret"), ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}
	before := chainRevision(t)

	inbounds := &InboundService{}
	if _, _, err := inbounds.AddInbound(&model.Inbound{
		UserId: 1, Enable: true, Port: 9443, Protocol: model.MTProto, Tag: "inbound-9443",
		Remark: "mtproto", Settings: `{"secret":"","domain":"example.com"}`,
	}); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	if after := chainRevision(t); after <= before {
		t.Errorf("chainRevision = %d after an inbound was added, want more than %d", after, before)
	}
}

// TestTogglingAnInboundMovesTheChainRevision: the enable switch is the one
// edit that changes which ports the panel serves without touching an inbound's
// fields, and a front that never hears about it keeps relaying a dead port.
func TestTogglingAnInboundMovesTheChainRevision(t *testing.T) {
	newPanel(t)
	registry := &ChainService{}
	hop, _, _, err := registry.Add(AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := registry.MarkJoined(hop.Id, chain.HashSecret("secret"), ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}

	inbounds := &InboundService{}
	inbound := &model.Inbound{
		UserId: 1, Enable: true, Port: 9443, Protocol: model.MTProto, Tag: "inbound-9443",
		Remark: "mtproto", Settings: `{"secret":"","domain":"example.com"}`,
	}
	if _, _, err := inbounds.AddInbound(inbound); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	before := chainRevision(t)

	if _, err := inbounds.SetInboundEnable(inbound.Id, false); err != nil {
		t.Fatalf("SetInboundEnable: %v", err)
	}
	after := chainRevision(t)
	if after <= before {
		t.Fatalf("chainRevision = %d after an inbound was switched off, want more than %d", after, before)
	}

	if _, err := inbounds.SetInboundEnable(inbound.Id, true); err != nil {
		t.Fatalf("SetInboundEnable back on: %v", err)
	}
	if again := chainRevision(t); again <= after {
		t.Errorf("chainRevision = %d after an inbound was switched on, want more than %d", again, after)
	}
}

// TestAddingAnInboundOnAPanelWithoutAChain leaves the revision alone: no box
// is polling, and a counter climbing on a panel that has no fronts is noise.
func TestAddingAnInboundOnAPanelWithoutAChain(t *testing.T) {
	newPanel(t)
	before := chainRevision(t)

	inbounds := &InboundService{}
	if _, _, err := inbounds.AddInbound(&model.Inbound{
		UserId: 1, Enable: true, Port: 9443, Protocol: model.MTProto, Tag: "inbound-9443",
		Remark: "mtproto", Settings: `{"secret":"","domain":"example.com"}`,
	}); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}

	if after := chainRevision(t); after != before {
		t.Errorf("chainRevision = %d on a panel with no chain, want it left at %d", after, before)
	}
}
