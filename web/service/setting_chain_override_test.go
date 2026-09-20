package service

import (
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// §2.3 — GetProxyOverride keeps its signature and its consumers, and changes
// where it looks: the active edge of the registry first, the legacy keys only
// when the registry has no active edge. Every generated config and
// subscription link hangs off this one answer.
func TestGetProxyOverrideIsDerivedFromTheRegistry(t *testing.T) {
	t.Run("legacy keys still work without a registry", func(t *testing.T) {
		s := newChainSettingService(t)
		setSetting(t, "proxyOverrideEnable", "true")
		setSetting(t, "proxyOverrideHost", "front.example.net")

		host, ok := s.GetProxyOverride()
		if !ok || host != "front.example.net" {
			t.Fatalf("GetProxyOverride = %q, %v; want front.example.net, true", host, ok)
		}
	})

	t.Run("a disabled legacy override stays off", func(t *testing.T) {
		s := newChainSettingService(t)
		setSetting(t, "proxyOverrideHost", "front.example.net")

		if host, ok := s.GetProxyOverride(); ok {
			t.Fatalf("GetProxyOverride = %q, true; want off", host)
		}
	})

	t.Run("the active edge wins over the legacy keys", func(t *testing.T) {
		s := newChainSettingService(t)
		setSetting(t, "proxyOverrideEnable", "true")
		setSetting(t, "proxyOverrideHost", "front.example.net")

		chainService := &ChainService{}
		edge := addJoined(t, chainService, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
		if err := chainService.SetActive(edge.Id); err != nil {
			t.Fatal(err)
		}

		host, ok := s.GetProxyOverride()
		if !ok || host != "a.example.net" {
			t.Fatalf("GetProxyOverride = %q, %v; want a.example.net, true", host, ok)
		}
	})

	t.Run("a registry without an active edge falls back", func(t *testing.T) {
		s := newChainSettingService(t)
		setSetting(t, "proxyOverrideEnable", "true")
		setSetting(t, "proxyOverrideHost", "front.example.net")

		chainService := &ChainService{}
		addJoined(t, chainService, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

		host, ok := s.GetProxyOverride()
		if !ok || host != "front.example.net" {
			t.Fatalf("GetProxyOverride = %q, %v; want the legacy host", host, ok)
		}
	})
}

// "/proxy off" must reach both halves: the registry's active edge and the
// legacy pair. Clearing one and leaving the other is the failure that makes
// the command look broken — the panel would keep publishing the edge.
func TestDisableProxyOverrideClearsBothHalves(t *testing.T) {
	s := newChainSettingService(t)
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")

	chainService := &ChainService{}
	edge := addJoined(t, chainService, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err := chainService.SetActive(edge.Id); err != nil {
		t.Fatal(err)
	}
	before := revisionOf(t, chainService)

	if err := s.DisableProxyOverride(); err != nil {
		t.Fatalf("DisableProxyOverride: %v", err)
	}

	if host, ok := s.GetProxyOverride(); ok {
		t.Fatalf("GetProxyOverride = %q, true; want off", host)
	}
	if enabled, _ := s.GetProxyOverrideEnable(); enabled {
		t.Error("the legacy enable flag is still on")
	}
	state, _ := chainService.List()
	if state.ActiveEdge != "" {
		t.Errorf("activeEdge = %q, want none", state.ActiveEdge)
	}
	if state.Revision != before+1 {
		t.Errorf("revision %d, want %d — clearing the active edge changes documents", state.Revision, before+1)
	}
	// The hop itself stays: turning the override off is not decommissioning.
	if len(state.Hops) != 1 {
		t.Errorf("registry holds %d hops, want the edge to survive", len(state.Hops))
	}
}
