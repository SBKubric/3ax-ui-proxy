package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/locale"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/text/language"
)

// initTestBotLocale loads the real translation files the way
// locale.InitLocalizer does, without an embed.FS: the point of these tests is
// that the tgbot.messages.monitoring.* keys exist and render, so the fixtures
// must be the shipped TOML and not a copy.
func initTestBotLocale(t *testing.T, lang string) {
	t.Helper()
	bundle := i18n.NewBundle(language.MustParse("en-US"))
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	entries, err := os.ReadDir(filepath.Join("..", "translation"))
	if err != nil {
		t.Fatalf("read translations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "translation", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := bundle.ParseMessageFileBytes(data, "translation/"+e.Name()); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
	}
	prev := locale.LocalizerBot
	locale.LocalizerBot = i18n.NewLocalizer(bundle, lang)
	t.Cleanup(func() { locale.LocalizerBot = prev })
}

// newTestBot is a Tgbot wired to the test database with Telegram replaced by
// a slice of captured messages.
func newTestBot(t *testing.T) (*Tgbot, *[]string) {
	t.Helper()
	var sent []string
	bot := &Tgbot{}
	bot.monSend = func(msg string) { sent = append(sent, msg) }
	return bot, &sent
}

func monEventRow(kind, client, inboundKind string, inboundId int, path, from, to, reason string, ts int64) model.MonEvent {
	return model.MonEvent{Id: "e-" + to + "-" + client, Ts: ts, Kind: kind, MonClientId: client,
		InboundKind: inboundKind, InboundId: inboundId, Path: path, From: from, To: to, Reason: reason}
}

// TestMonitoringEventMessages covers every transition §6 names, the
// "down for" part taken from the preceding DOWN event, and the transitions
// the panel settles without a word.
func TestMonitoringEventMessages(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	monInbound(t, 1, model.VLESS, true)
	monRemark(t, 1, "Reality main")
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"})

	base := monSummaryNow.UnixMilli()
	// Two earlier events the duration of a recovery is measured from.
	monStoreEvent(t, ev1, base-7*60000, model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", "UP", "DOWN")
	monStoreEvent(t, ev2, base-75*60000, model.MonEventKindMonClient, "ams-1", "", 0, "", "ONLINE", "OFFLINE")
	since := time.UnixMilli(base).Format("15:04")

	tests := []struct {
		name   string
		event  model.MonEvent
		silent bool
		want   []string
		absent []string
	}{
		{
			name:  "target down",
			event: monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", "UP", model.MonStateDown, "tls_timeout", base),
			want:  []string{"DOWN", "Reality main", "via proxy", "Amsterdam (eu-west)", "reason: tls_timeout", "since " + since},
		},
		{
			name:  "target up carries how long it was down",
			event: monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateDown, model.MonStateUp, "", base),
			want:  []string{"UP", "Reality main", "down for 7m"},
		},
		{
			name:   "target up without a preceding down says nothing about duration",
			event:  monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "direct", model.MonStateDown, model.MonStateUp, "", base),
			want:   []string{"UP", "Reality main", "via direct"},
			absent: []string{"down for"},
		},
		{
			name:  "flapping on",
			event: monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateUp, model.MonStateFlapping, "packet_loss", base),
			want:  []string{"FLAPPING", "reason: packet_loss"},
		},
		{
			name:  "flapping off",
			event: monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateFlapping, model.MonStateUp, "", base),
			want:  []string{"FLAPPING over", model.MonStateUp},
		},
		{
			name:  "unknown inbound falls back to its monitoring key",
			event: monEventRow(model.MonEventKindTarget, "ams-1", "xray", 77, "proxy", model.MonStateUp, model.MonStateDown, "tcp_refused", base),
			want:  []string{"xray#77"},
		},
		{
			name:  "unknown mon-client is printed as it came",
			event: monEventRow(model.MonEventKindTarget, "sgp-9", "xray", 1, "proxy", model.MonStateUp, model.MonStateDown, "tcp_refused", base),
			want:  []string{"sgp-9"},
		},
		{
			name:  "mon-client offline",
			event: monEventRow(model.MonEventKindMonClient, "ams-1", "", 0, "", "ONLINE", "OFFLINE", "", base),
			want:  []string{"mon-client", "Amsterdam (eu-west)", "OFFLINE", "since " + since},
		},
		{
			name:  "mon-client online carries how long it was away",
			event: monEventRow(model.MonEventKindMonClient, "ams-1", "", 0, "", "OFFLINE", "ONLINE", "", base),
			want:  []string{"mon-client", "ONLINE", "was offline 1h 15m"},
		},
		{
			name:   "transition to UNKNOWN is settled silently",
			event:  monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateUp, model.MonStateUnknown, "", base),
			silent: true,
		},
		{
			name:   "transition to PAUSED is settled silently",
			event:  monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateUp, model.MonStatePaused, "", base),
			silent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot, sent := newTestBot(t)
			ids := bot.NotifyMonitoringEvents([]model.MonEvent{tc.event})
			if len(ids) != 1 || ids[0] != tc.event.Id {
				t.Fatalf("ids = %v, want the event settled even when nothing is sent", ids)
			}
			if tc.silent {
				if len(*sent) != 0 {
					t.Fatalf("sent %q, want silence", *sent)
				}
				return
			}
			if len(*sent) != 1 {
				t.Fatalf("sent %d messages, want exactly one: %q", len(*sent), *sent)
			}
			msg := (*sent)[0]
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(msg, absent) {
					t.Errorf("message %q should not contain %q", msg, absent)
				}
			}
		})
	}
}

// TestMonitoringStaleMessages: both edges of the panel's own state reach
// Telegram with the numbers §6 asks for.
func TestMonitoringStaleMessages(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	bot, sent := newTestBot(t)

	bot.NotifyMonitoringStale(monSummaryNow)
	bot.NotifyMonitoringBack(95 * time.Minute)
	if len(*sent) != 2 {
		t.Fatalf("sent %d messages, want 2: %q", len(*sent), *sent)
	}
	if !strings.Contains((*sent)[0], "silent since "+monSummaryNow.Format("15:04")) {
		t.Errorf("stale message = %q", (*sent)[0])
	}
	if !strings.Contains((*sent)[1], "was silent 95 min") {
		t.Errorf("back message = %q", (*sent)[1])
	}
}

// TestNotifyMonitoringEventsWithoutABot: with no bot running nothing is
// settled, so the events stay notified=false and remain visible as
// un-notified in the feed instead of being silently swallowed.
func TestNotifyMonitoringEventsWithoutABot(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	bot := &Tgbot{} // no monSend, IsRunning() is false
	ev := monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, "proxy", model.MonStateUp, model.MonStateDown, "tls_timeout", monSummaryNow.UnixMilli())
	if ids := bot.NotifyMonitoringEvents([]model.MonEvent{ev}); ids != nil {
		t.Errorf("ids = %v, want nil while the bot is down", ids)
	}
}

// TestApplyEventsOffersOnlyFreshTransitionsToTheBot wires the bot in as the
// real notifier: a panel event and an event mon-server already notified are
// not offered, a DOWN transition is, and the row it delivered comes back
// notified. Then the same batch with the bot down leaves the row unnotified.
func TestApplyEventsOffersOnlyFreshTransitionsToTheBot(t *testing.T) {
	m := newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	monInbound(t, 1, model.VLESS, true)
	monRemark(t, 1, "Reality main")
	bot, sent := newTestBot(t)
	SetMonEventNotifier(bot)
	t.Cleanup(func() { SetMonEventNotifier(nil) })

	base := monSummaryNow.UnixMilli()
	batch := []MonEventIn{
		targetEvent(ev1, base, "UP", model.MonStateDown, "tls_timeout"),
		{Id: ev2, Ts: base + 1, Kind: model.MonEventKindPanel, To: "PANEL_DOWN"},
		{Id: ev3, Ts: base + 2, Kind: model.MonEventKindTarget, MonClientId: "ams-1", InboundKind: "xray", InboundId: 1,
			Path: "direct", From: "UP", To: model.MonStateDown, Reason: "tcp_refused", Notified: true},
	}
	if _, err := m.ApplyEvents(batch); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "via proxy") {
		t.Fatalf("sent %q, want only the fresh proxy DOWN", *sent)
	}
	var delivered model.MonEvent
	if err := database.GetDB().First(&delivered, "id = ?", ev1).Error; err != nil {
		t.Fatalf("load %s: %v", ev1, err)
	}
	if !delivered.Notified {
		t.Error("a delivered event was not marked notified")
	}

	// The same transition on another target while the bot is down.
	SetMonEventNotifier(&Tgbot{})
	if _, err := m.ApplyEvents([]MonEventIn{targetEvent(ev4, base+3, model.MonStateDown, model.MonStateUp, "")}); err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	var undelivered model.MonEvent
	if err := database.GetDB().First(&undelivered, "id = ?", ev4).Error; err != nil {
		t.Fatalf("load %s: %v", ev4, err)
	}
	if undelivered.Notified {
		t.Error("an event nobody could deliver was marked notified")
	}
}

// TestMonitoringDigest: the Monitoring block of SendReport prints a title, a
// line per enabled inbound (the "no data" variant included), the worst
// target, the offline mon-clients and the silence of the window.
func TestMonitoringDigest(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	resetMonStaleForTest()
	t.Cleanup(resetMonStaleForTest)
	monInbound(t, 1, model.VLESS, true)
	monInbound(t, 2, model.VLESS, true)
	monRemark(t, 1, "Reality main")
	monRemark(t, 2, "Reality spare")
	monRegister(t,
		MonClient{Id: "ams-1", Name: "Amsterdam", Region: "eu-west", State: "ONLINE"},
		MonClient{Id: "fra-1", Name: "Frankfurt", Region: "eu-central", State: "OFFLINE"},
		MonClient{Id: "new-1", Name: "Newcomer", Region: "pl", State: "NEVER"},
	)

	// A minute inside the window, so the sliding start of Summary cannot clip
	// the first bucket away.
	from := time.Now().Add(-24*time.Hour).UnixMilli() + 60000
	monCurrent(t, "ams-1", "xray", 1, "direct", from, 150, 9, 1)
	monCurrent(t, "fra-1", "xray", 1, "direct", from, 150, 10, 0)
	monCurrent(t, "ams-1", "xray", 1, "proxy", from, 287, 10, 0)
	monStoreEvent(t, ev1, from+1000, model.MonEventKindTarget, "ams-1", "xray", 1, "direct", "UP", "DOWN")

	// A silence that ended inside the window.
	monStale.Lock()
	monStale.intervals = []monStaleInterval{{from: from + 60000, to: from + 13*60000}}
	monStale.Unlock()

	bot, _ := newTestBot(t)

	if digest := bot.monitoringDigest(); digest != "" {
		t.Fatalf("digest with monitoring disabled = %q, want empty", digest)
	}
	if err := (&SettingService{}).SetMonEnable(true); err != nil {
		t.Fatalf("enable monitoring: %v", err)
	}

	digest := bot.monitoringDigest()
	for _, want := range []string{
		"Monitoring",
		"Reality main: direct 95.0 % · proxy 100.0 % (cov ",
		"· 1 incidents",
		"Reality spare: no data",
		"Worst: Reality main via direct · Amsterdam (eu-west) · 90.0 % (cov ",
		"mon-clients offline: Frankfurt (eu-central)\r\n",
		"mon-clients awaiting first heartbeat: Newcomer (pl)",
		"Monitoring silent: 12 min",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest\n%s\ndoes not contain %q", digest, want)
		}
	}
}

// TestMonitoringKeysPresentInEveryLocale: go-i18n does not fall back to
// en_US for a key missing from a locale that exists, so the other eleven
// locales carry an English copy of the monitoring keys. This pins that a
// bot set to any of them renders a message instead of an empty string.
func TestMonitoringKeysPresentInEveryLocale(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "fa-IR")
	bot, _ := newTestBot(t)
	msg := bot.I18nBot("tgbot.messages.monitoring.stale", "Since==10:00")
	if !strings.Contains(msg, "silent since 10:00") {
		t.Errorf("fa-IR message = %q, want the English copy of the key", msg)
	}

	initTestBotLocale(t, "ru-RU")
	if msg := bot.I18nBot("tgbot.messages.monitoring.stale", "Since==10:00"); !strings.Contains(msg, "10:00") {
		t.Errorf("ru-RU message = %q", msg)
	}
}
