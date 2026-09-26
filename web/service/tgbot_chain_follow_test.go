package service

import (
	"strings"
	"testing"

	"github.com/mymmrac/telego"
)

// /proxy with chain-following inbounds (#139): a switch changes the SNI in
// their links, so the bot asks before it switches, and the switch happens only
// when the owner presses the button.

// followingBot is chainBotFixture with neighbour targets on both joined edges
// and one inbound following the chain.
func followingBot(t *testing.T) *Tgbot {
	t.Helper()
	bot := chainBotFixture(t)
	s := &ChainService{}
	state, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range []string{"edge-a", "edge-b"} {
		hop := findChainHop(state, name)
		if err := s.Update(hop.Id, UpdateHopInput{RealityTarget: strRef("www.neighbour-" + name + ".example:443")}); err != nil {
			t.Fatalf("Update(%s): %v", name, err)
		}
	}
	seedInbound(t, 24443, true, followStream)
	return bot
}

func activeEdgeName(t *testing.T) string {
	t.Helper()
	state, err := (&ChainService{}).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return state.ActiveEdge
}

// callbackOf returns the callback data of the keyboard's only button.
func callbackOf(t *testing.T, keyboard *telego.InlineKeyboardMarkup) string {
	t.Helper()
	if keyboard == nil || len(keyboard.InlineKeyboard) != 1 || len(keyboard.InlineKeyboard[0]) != 1 {
		t.Fatalf("want a keyboard with one button, got %+v", keyboard)
	}
	button := keyboard.InlineKeyboard[0][0]
	if button.Text != "Switch" {
		t.Errorf("button text %q, want Switch", button.Text)
	}
	return button.CallbackData
}

func TestProxySwitchWithFollowersAsksFirst(t *testing.T) {
	bot := followingBot(t)

	text, keyboard := bot.chainProxyCommand([]string{"edge-b"})
	want := "⚠️ Switch the active edge to <code>edge-b</code>?\r\n" +
		"Clients need to refresh their subscription: the old links will stop working."
	if text != want {
		t.Fatalf("reply:\n got %q\nwant %q", text, want)
	}
	if data := callbackOf(t, keyboard); data != "chain_switch edge-b" {
		t.Fatalf("callback data %q, want chain_switch edge-b", data)
	}
	if active := activeEdgeName(t); active != "edge-a" {
		t.Fatalf("active edge is %q before the button was pressed", active)
	}

	// Pressing the button is what switches.
	got := bot.chainSwitchConfirmed("edge-b")
	want = "✅ Active edge is now <code>edge-b</code>.\r\n" +
		"Clients need to refresh their subscription: the old links will stop working."
	if got != want {
		t.Fatalf("confirmed switch:\n got %q\nwant %q", got, want)
	}
	if active := activeEdgeName(t); active != "edge-b" {
		t.Fatalf("active edge is %q after the button, want edge-b", active)
	}
}

// The button can be pressed long after it was offered: the registry is asked
// again, and a hop that can no longer be switched to gets the listing.
func TestProxySwitchCallbackRevalidates(t *testing.T) {
	bot := followingBot(t)
	setChainHopFields(t, "edge-b", map[string]any{"state": "pending"})

	got := bot.chainSwitchConfirmed("edge-b")
	if !strings.HasPrefix(got, "❗ No hop named <code>edge-b</code>.") {
		t.Fatalf("a stale button switched or said something else: %q", got)
	}
	if active := activeEdgeName(t); active != "edge-a" {
		t.Fatalf("active edge is %q, want edge-a untouched", active)
	}
}

// An edge without a neighbour target cannot take over while inbounds follow
// the chain; the bot says so instead of offering a button that would fail.
func TestProxySwitchWithFollowersNeedsANeighbourTarget(t *testing.T) {
	bot := followingBot(t)
	setChainHopFields(t, "edge-legacy", map[string]any{"reality_target": ""})

	text, keyboard := bot.chainProxyCommand([]string{"edge-legacy"})
	want := "❗ <code>edge-legacy</code> has no neighbour target, and inbounds follow the chain. Set its target in the chain editor first."
	if text != want {
		t.Fatalf("reply:\n got %q\nwant %q", text, want)
	}
	if keyboard != nil {
		t.Fatal("a button was offered for a switch that would be refused")
	}
}

// /proxy off leaves the followers alone and says so.
func TestProxyOffWithFollowersSaysTheyStay(t *testing.T) {
	bot := followingBot(t)
	text, keyboard := bot.chainProxyCommand([]string{"off"})
	want := "✅ Proxy chain override disabled.\r\n" +
		"⚠️ Chain-following inbounds keep the last edge's neighbour target: their links keep working, only the host changes."
	if text != want {
		t.Fatalf("reply:\n got %q\nwant %q", text, want)
	}
	if keyboard != nil {
		t.Fatal("/proxy off offered a button")
	}
}
