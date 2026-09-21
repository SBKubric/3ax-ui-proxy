package service

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// chainBotFixture is the registry behind the §7.6 mock: one joined inner and
// four edges spanning every state and marker the listing has to render.
//
//	core-1  inner  joined
//	edge-a  edge   joined  active   health UP
//	edge-b  edge   joined           health DOWN
//	edge-c  edge   pending          health UNKNOWN
//	edge-legacy edge legacy         health UNKNOWN
//
// Every hop's lastRevision is set to the fixed chainRevision (42), so every
// line reads "fresh": freshness lag is exercised in a test of its own.
func chainBotFixture(t *testing.T) *Tgbot {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	chainSvc := &ChainService{}
	addChainHop(t, chainSvc, AddHopInput{Name: "core-1", Host: "10.0.0.7", Role: chain.RoleInner})
	addChainHop(t, chainSvc, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	addChainHop(t, chainSvc, AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})
	addChainHop(t, chainSvc, AddHopInput{Name: "edge-c", Host: "c.example.net", Role: chain.RoleEdge})
	addChainHop(t, chainSvc, AddHopInput{Name: "edge-legacy", Host: "legacy.example.net", Role: chain.RoleEdge})

	setChainHopFields(t, "core-1", map[string]any{"state": chain.StateJoined})
	setChainHopFields(t, "edge-a", map[string]any{"state": chain.StateJoined, "is_active": true})
	setChainHopFields(t, "edge-b", map[string]any{"state": chain.StateJoined})
	setChainHopFields(t, "edge-legacy", map[string]any{"state": chain.StateLegacy})
	// edge-c stays pending.

	settingSvc := &SettingService{}
	if err := settingSvc.SetChainRevision(42); err != nil {
		t.Fatalf("SetChainRevision: %v", err)
	}
	if err := database.GetDB().Model(&model.ChainHop{}).Where("1 = 1").
		Update("last_revision", int64(42)).Error; err != nil {
		t.Fatalf("set last_revision: %v", err)
	}

	prevHealth := chainHealthLookup
	chainHealthLookup = func(name string) string {
		switch name {
		case "edge-a":
			return "UP"
		case "edge-b":
			return "DOWN"
		default:
			return "UNKNOWN"
		}
	}
	t.Cleanup(func() { chainHealthLookup = prevHealth })

	initTestBotLocale(t, "en-US")
	bot, _ := newTestBot(t)
	return bot
}

func addChainHop(t *testing.T, s *ChainService, in AddHopInput) {
	t.Helper()
	if _, _, _, err := s.Add(in); err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
}

func setChainHopFields(t *testing.T, name string, fields map[string]any) {
	t.Helper()
	if err := database.GetDB().Model(&model.ChainHop{}).Where("name = ?", name).
		Updates(fields).Error; err != nil {
		t.Fatalf("update hop %q: %v", name, err)
	}
}

const chainListingEnUS = "🔗 <b>Proxy chain</b> (revision 42)\r\n\r\n" +
	"· <code>core-1</code> · inner · joined · fresh · UNKNOWN\r\n" +
	"★ <code>edge-a</code> · edge · joined · fresh · UP\r\n" +
	"· <code>edge-b</code> · edge · joined · fresh · DOWN\r\n" +
	"· <code>edge-c</code> · edge · pending · fresh · UNKNOWN\r\n" +
	"· <code>edge-legacy</code> · edge · legacy · fresh · UNKNOWN\r\n\r\n" +
	"Switch the active edge: <code>/proxy &lt;name&gt;</code>"

// TestChainProxyCommand covers §7.6's mock output against a seeded registry:
// the bare listing, switching to a joined-but-inactive edge, an unknown name,
// switching to an inner or a pending hop (same as unknown, §7.6's closing
// note), and /proxy off.
func TestChainProxyCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		// wantActiveEdge, when set, is checked against the registry's active
		// edge after chainProxyCommand runs — for the switch cases, on top of
		// the message itself.
		wantActiveEdge string
	}{
		{
			name: "bare listing",
			args: nil,
			want: chainListingEnUS,
		},
		{
			name: "switch to a joined edge",
			args: []string{"edge-b"},
			want: "✅ Active edge is now <code>edge-b</code>.\r\n" +
				"Links already handed out keep using the previous edge until clients refresh the subscription.",
			wantActiveEdge: "edge-b",
		},
		{
			// A legacy hop is as switchable as a joined one (§2.7 invariant
			// 1: active is joined OR legacy) — the pre-chain override host,
			// imported at startup, is still a valid /proxy <name> target.
			name: "switch to a legacy hop",
			args: []string{"edge-legacy"},
			want: "✅ Active edge is now <code>edge-legacy</code>.\r\n" +
				"Links already handed out keep using the previous edge until clients refresh the subscription.",
			wantActiveEdge: "edge-legacy",
		},
		{
			name: "unknown name falls back to the listing",
			args: []string{"nope"},
			want: "❗ No hop named <code>nope</code>. Showing the chain instead.\r\n\r\n" + chainListingEnUS,
		},
		{
			name: "an inner front cannot become active",
			args: []string{"core-1"},
			want: "❗ No hop named <code>core-1</code>. Showing the chain instead.\r\n\r\n" + chainListingEnUS,
		},
		{
			name: "a pending hop cannot become active",
			args: []string{"edge-c"},
			want: "❗ No hop named <code>edge-c</code>. Showing the chain instead.\r\n\r\n" + chainListingEnUS,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot := chainBotFixture(t)
			got := bot.chainProxyCommand(tc.args)
			if got != tc.want {
				t.Fatalf("chainProxyCommand(%v):\n got %q\nwant %q", tc.args, got, tc.want)
			}
			if tc.wantActiveEdge != "" {
				state, err := (&ChainService{}).List()
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if state.ActiveEdge != tc.wantActiveEdge {
					t.Fatalf("active edge is %q, want %q", state.ActiveEdge, tc.wantActiveEdge)
				}
			}
		})
	}
}

// TestChainProxyCommandOff covers /proxy off: it clears the active edge, the
// same way DisableProxyOverride always has.
func TestChainProxyCommandOff(t *testing.T) {
	bot := chainBotFixture(t)
	got := bot.chainProxyCommand([]string{"off"})
	want := "✅ Proxy chain override disabled."
	if got != want {
		t.Fatalf("chainProxyCommand(off):\n got %q\nwant %q", got, want)
	}
	state, err := (&ChainService{}).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if state.ActiveEdge != "" {
		t.Fatalf("active edge is still %q after /proxy off", state.ActiveEdge)
	}
}

// TestChainProxyCommandEmptyRegistry covers the fresh-install case: no hop has
// ever been added, so the legacy host override has nothing to migrate and the
// listing prints empty.
func TestChainProxyCommandEmptyRegistry(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	initTestBotLocale(t, "en-US")
	bot, _ := newTestBot(t)

	got := bot.chainProxyCommand(nil)
	want := "🔗 <b>Proxy chain</b> (revision 0)\r\n\r\n" +
		"No hops in the registry yet.\r\n\r\n" +
		"Switch the active edge: <code>/proxy &lt;name&gt;</code>"
	if got != want {
		t.Fatalf("chainProxyCommand(nil) on an empty registry:\n got %q\nwant %q", got, want)
	}
}

// TestChainProxyCommandStaleFreshness covers the "rev N" branch of §7.4's
// freshness marker: a hop whose lastRevision trails the registry's current
// one.
func TestChainProxyCommandStaleFreshness(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	chainSvc := &ChainService{}
	addChainHop(t, chainSvc, AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	setChainHopFields(t, "edge-a", map[string]any{"state": chain.StateJoined, "last_revision": int64(39)})
	settingSvc := &SettingService{}
	if err := settingSvc.SetChainRevision(42); err != nil {
		t.Fatalf("SetChainRevision: %v", err)
	}

	prevHealth := chainHealthLookup
	chainHealthLookup = func(name string) string { return "UNKNOWN" }
	t.Cleanup(func() { chainHealthLookup = prevHealth })

	initTestBotLocale(t, "en-US")
	bot, _ := newTestBot(t)

	got := bot.chainProxyCommand(nil)
	want := "🔗 <b>Proxy chain</b> (revision 42)\r\n\r\n" +
		"· <code>edge-a</code> · edge · joined · rev 39 · UNKNOWN\r\n\r\n" +
		"Switch the active edge: <code>/proxy &lt;name&gt;</code>"
	if got != want {
		t.Fatalf("chainProxyCommand(nil) with a stale hop:\n got %q\nwant %q", got, want)
	}
}
