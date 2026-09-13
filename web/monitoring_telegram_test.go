package web

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/locale"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// The formatter is exercised here rather than in the service package because
// the translations are embedded in this one.

func initBotLocale(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	service.ResetMonRuntime()
	t.Cleanup(service.ResetMonRuntime)
	settings := &service.SettingService{} // tgLang defaults to en-US
	if err := locale.InitLocalizer(i18nFS, settings); err != nil {
		t.Fatalf("InitLocalizer: %v", err)
	}
}

func note(kind, to, prev string, prevSince int64) service.MonNotification {
	return service.MonNotification{
		Event: model.MonEvent{Id: "0192", Ts: 1_757_721_540_000, Kind: kind, MonClientId: "ams-1", InboundKind: "xray",
			InboundId: 12, Path: "proxy", FromState: prev, ToState: to, Reason: "tls_timeout"},
		InboundRemark: "Reality main",
		MonClient:     service.MonClient{Id: "ams-1", Name: "Amsterdam #1", Region: "NL"},
		PrevState:     prev,
		PrevSince:     prevSince,
	}
}

// TestMonitoringEventMessages: every announced transition has the shape of
// spec §6, and the ones the panel keeps quiet about produce nothing.
func TestMonitoringEventMessages(t *testing.T) {
	initBotLocale(t)
	bot := &service.Tgbot{}
	since := time.UnixMilli(1_757_721_540_000)
	sinceClock := since.In(time.Local).Format("15:04")

	cases := []struct {
		name string
		n    service.MonNotification
		want []string
	}{
		{"down", note("target", "DOWN", "UP", 0), []string{"⛔ DOWN", "Reality main via proxy", "Amsterdam #1 (NL)", "reason: tls_timeout", "since " + sinceClock}},
		{"up after down", note("target", "UP", "DOWN", since.Add(-7*time.Minute).UnixMilli()), []string{"✅ UP", "Reality main via proxy", "Amsterdam #1 (NL)", "down for 7m"}},
		{"flapping on", note("target", "FLAPPING", "UP", 0), []string{"〰 FLAPPING", "Reality main via proxy", "reason: tls_timeout"}},
		{"flapping off", note("target", "UP", "FLAPPING", 0), []string{"FLAPPING ended", "now UP"}},
		{"client offline", note("mon_client", "OFFLINE", "ONLINE", 0), []string{"🔴 mon-client Amsterdam #1 (NL) OFFLINE"}},
		{"client online", note("mon_client", "ONLINE", "OFFLINE", 0), []string{"🟢 mon-client Amsterdam #1 (NL) ONLINE"}},
	}
	for _, c := range cases {
		got := bot.MonitoringEventMessage(c.n)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
	}
	up := bot.MonitoringEventMessage(note("target", "UP", "UNKNOWN", 0))
	if strings.Contains(up, "down for") {
		t.Errorf("UP from UNKNOWN must not claim a downtime: %q", up)
	}
	for _, quiet := range []service.MonNotification{
		note("target", "PAUSED", "UP", 0),
		note("target", "UNKNOWN", "UP", 0),
		note("panel", "PANEL_DOWN", "PANEL_UP", 0),
	} {
		if got := bot.MonitoringEventMessage(quiet); got != "" {
			t.Errorf("%s -> %s should be silent, got %q", quiet.Event.Kind, quiet.Event.ToState, got)
		}
	}
	// A mon-client with no name falls back to its id.
	bare := note("mon_client", "OFFLINE", "ONLINE", 0)
	bare.MonClient = service.MonClient{Id: "msk-1"}
	if got := bot.MonitoringEventMessage(bare); !strings.Contains(got, "mon-client msk-1 OFFLINE") {
		t.Errorf("id fallback: %q", got)
	}

	// STALE and back are formatted from the same keys; the online message
	// says how long a mon-client was away when the feed knows.
	if got := locale.I18n(locale.Bot, "tgbot.messages.monitoring.stale", "Since=="+sinceClock); !strings.Contains(got, "monitoring silent since "+sinceClock) {
		t.Errorf("stale: %q", got)
	}
	if got := locale.I18n(locale.Bot, "tgbot.messages.monitoring.staleBack", "SilentFor==17m"); !strings.Contains(got, "was silent 17m") {
		t.Errorf("staleBack: %q", got)
	}
	if err := database.GetDB().Create(&model.MonEvent{Id: "00000000-0000-7000-8000-000000000001", Ts: since.Add(-5 * time.Minute).UnixMilli(),
		Kind: "mon_client", MonClientId: "ams-1", ToState: "OFFLINE"}).Error; err != nil {
		t.Fatal(err)
	}
	if got := bot.MonitoringEventMessage(note("mon_client", "ONLINE", "OFFLINE", 0)); !strings.Contains(got, "(was offline 5m)") {
		t.Errorf("online with a known offline start: %q", got)
	}
}

// TestMonitoringDigest: the report block lists every inbound with data,
// uptime per path with coverage when it is partial, incidents, the worst
// target, OFFLINE mon-clients and the STALE time; it is empty while
// monitoring is off.
func TestMonitoringDigest(t *testing.T) {
	initBotLocale(t)
	bot := &service.Tgbot{}
	settings := &service.SettingService{}
	if got := bot.MonitoringDigest(); got != "" {
		t.Fatalf("digest with monitoring off: %q", got)
	}
	if err := settings.SetMonEnable(true); err != nil {
		t.Fatal(err)
	}
	if got := bot.MonitoringDigest(); got != "" {
		t.Fatalf("digest before any contact or data: %q", got)
	}

	db := database.GetDB()
	ib := &model.Inbound{UserId: 1, Remark: "Reality main", Enable: true, Port: 443, Protocol: model.VLESS, Tag: "inbound-443",
		Settings: `{"clients":[]}`, StreamSettings: `{"network":"tcp","security":"none"}`}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}
	// The probe is seeded by hand: xray is not running here, so ensure has
	// nothing left to create and only refreshes the registry snapshot.
	probe := `{"clients":[{"id":"11111111-1111-1111-1111-111111111111","email":"` + service.ProbeEmail("xray", ib.Id) + `","enable":true}]}`
	if err := db.Model(ib).Update("settings", probe).Error; err != nil {
		t.Fatal(err)
	}
	m := &service.MonitoringService{}
	if _, err := m.EnsureProbeSet([]service.MonClient{{Id: "ams-1", Name: "Amsterdam #1", Region: "NL", State: "ONLINE"},
		{Id: "msk-1", Name: "Moscow #1", Region: "RU", State: "OFFLINE"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	bucket := now.Add(-14 * time.Hour).UnixMilli()
	bucket -= bucket % 300_000
	var stats []service.MonStatIn
	for i := 0; i < 160; i++ { // 160 of the 288 buckets of a day (55.6 %): direct all good
		stats = append(stats, service.MonStatIn{MonClientId: "ams-1", InboundKind: "xray", InboundId: ib.Id, Path: "direct",
			BucketStart: bucket + int64(i)*300_000, NOk: 5, NFail: 0})
	}
	for i := 0; i < 160; i++ { // proxy: one failure in four
		stats = append(stats, service.MonStatIn{MonClientId: "ams-1", InboundKind: "xray", InboundId: ib.Id, Path: "proxy",
			BucketStart: bucket + int64(i)*300_000, NOk: 3, NFail: 1})
	}
	if _, err := m.UpsertStats(&service.MonStatsBatch{Stats: stats}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyEvents(&service.MonEventsBatch{Events: []service.MonEventIn{
		{Id: "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", Ts: bucket + 600_000, Kind: "target", MonClientId: "ams-1", InboundKind: "xray",
			InboundId: ib.Id, Path: "proxy", From: "UP", To: "DOWN", Reason: "tls_timeout", Notified: true},
	}}); err != nil {
		t.Fatal(err)
	}
	m.TouchLastContact(now.Add(-40 * time.Minute))
	m.CheckStale(now.Add(-20 * time.Minute))
	m.RecordContact(now.Add(-10 * time.Minute))

	got := bot.MonitoringDigest()
	for _, w := range []string{
		"📡 Monitoring, last 24h",
		"Reality main: direct 100.0% (cov 55.6%) · proxy 75.0% (cov 55.6%) · 1 incidents",
		"Worst target: Reality main via proxy · Amsterdam #1 (NL) · 75.0%",
		"mon-clients OFFLINE now: Moscow #1 (RU)",
		"monitoring was silent for 30m",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("digest lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "no probe data") {
		t.Errorf("digest claims no data:\n%s", got)
	}
}
