package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

func initProbeTestDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
}

func TestIsProbeAccount(t *testing.T) {
	for email, want := range map[string]bool{
		"probe-12":        true,
		"probe-awg":       true,
		"Probe-12":        true,
		"PROBE-x":         true,
		"probe-":          true,
		"probe":           false,
		"probe12":         false,
		"alice":           false,
		"":                false,
		"my-probe-12":     false,
		"prob-e-12":       false,
		"probe_12":        false,
		"pro":             false,
		ProbeXrayEmail(7): true,
	} {
		if got := IsProbeAccount(email); got != want {
			t.Errorf("IsProbeAccount(%q) = %v, want %v", email, got, want)
		}
	}
}

func TestNewProbeClientsCarryTheSpecAttributes(t *testing.T) {
	c := NewProbeXrayClient(12, "sub0123456789abc", "xtls-rprx-vision")
	if c.Email != "probe-12" || !c.Enable || c.TotalGB != 0 || c.ExpiryTime != 0 || c.LimitIP != 0 ||
		c.Reset != 0 || c.TgID != 0 || c.SubID != "sub0123456789abc" || c.Comment != ProbeComment || c.Flow != "xtls-rprx-vision" {
		t.Errorf("xray probe client: %+v", c)
	}
	tc := NewProbeTunnelClient()
	if tc.Email != "probe-awg" || tc.Name != "probe-awg" || !tc.Enable || tc.Comment != ProbeComment {
		t.Errorf("tunnel probe client: %+v", tc)
	}
}

// vlessInbound stores a vless inbound with the given clients and returns it.
// Disabled, so the client paths never reach for the xray API.
func vlessInbound(t *testing.T, id int, clients ...model.Client) *model.Inbound {
	t.Helper()
	settings, _ := json.Marshal(map[string]any{"clients": clients, "decryption": "none"})
	ib := &model.Inbound{Id: id, Port: 10000 + id, Protocol: model.VLESS, Tag: "inbound-" + strings.Repeat("x", id%3) + ProbeXrayEmail(id),
		Remark: "t", Settings: string(settings), Enable: false, StreamSettings: "{}", Sniffing: "{}"}
	db := database.GetDB()
	if err := db.Create(ib).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
	// Every client has a client_traffics row, as it would after AddInboundClient.
	for _, c := range clients {
		if err := db.Create(&xray.ClientTraffic{InboundId: id, Email: c.Email, Enable: true}).Error; err != nil {
			t.Fatalf("create client traffic: %v", err)
		}
	}
	return ib
}

func clientsPayload(id int, clients ...model.Client) *model.Inbound {
	settings, _ := json.Marshal(map[string]any{"clients": clients})
	return &model.Inbound{Id: id, Settings: string(settings)}
}

// TestProbeEmailRefusedOnXrayClientPaths: neither the panel's add path nor its
// update path accepts a probe email, in either direction of a rename.
func TestProbeEmailRefusedOnXrayClientPaths(t *testing.T) {
	initProbeTestDB(t)
	s := &InboundService{}
	alice := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice", Enable: false}
	probe := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: "probe-1", Enable: false}
	vlessInbound(t, 1, alice, probe)

	// Add: a probe email anywhere in the batch fails the batch.
	bob := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000003", Email: "bob", Enable: false}
	newProbe := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000004", Email: "Probe-9", Enable: false}
	if _, err := s.AddInboundClient(clientsPayload(1, bob, newProbe)); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("AddInboundClient with a probe email: err = %v, want the probe guard", err)
	}
	if clients, _ := s.GetClients(mustInbound(t, s, 1)); len(clients) != 2 {
		t.Errorf("the refused batch was partly applied: %d clients", len(clients))
	}
	// ...and a regular add still works.
	if _, err := s.AddInboundClient(clientsPayload(1, bob)); err != nil {
		t.Fatalf("AddInboundClient(bob): %v", err)
	}

	// Update: editing the probe is refused even when the email is kept.
	edited := probe
	edited.Comment = "edited"
	if _, err := s.UpdateInboundClient(clientsPayload(1, edited), probe.ID); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("UpdateInboundClient(probe): err = %v, want the probe guard", err)
	}
	// Renaming the probe into a user is refused.
	renamed := probe
	renamed.Email = "carol"
	if _, err := s.UpdateInboundClient(clientsPayload(1, renamed), probe.ID); err == nil {
		t.Error("UpdateInboundClient renaming a probe away succeeded")
	}
	// Renaming a user into a probe is refused.
	aliceAsProbe := alice
	aliceAsProbe.Email = "probe-1000"
	if _, err := s.UpdateInboundClient(clientsPayload(1, aliceAsProbe), alice.ID); err == nil {
		t.Error("UpdateInboundClient renaming a user into a probe succeeded")
	}
	// A regular update still works.
	aliceEdited := alice
	aliceEdited.Comment = "vip"
	if _, err := s.UpdateInboundClient(clientsPayload(1, aliceEdited), alice.ID); err != nil {
		t.Fatalf("UpdateInboundClient(alice): %v", err)
	}

	// Delete stays open: the next ensure recreates the probe.
	if _, err := s.DelInboundClient(1, probe.ID); err != nil {
		t.Fatalf("DelInboundClient(probe): %v", err)
	}
	for _, c := range mustClients(t, s, 1) {
		if c.Email == "probe-1" {
			t.Error("probe still present after delete")
		}
	}
}

// TestProbeEmailRefusedFromTelegramBot: the bot's add-client dialogue ends in
// the same service path, so it is guarded too.
func TestProbeEmailRefusedFromTelegramBot(t *testing.T) {
	initProbeTestDB(t)
	vlessInbound(t, 2, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000011", Email: "dave"})

	receiver_inbound_ID = 2
	client_Id = "aaaaaaaa-0000-0000-0000-000000000012"
	client_Email = "probe-2"
	client_Enable = false
	t.Cleanup(func() { receiver_inbound_ID, client_Id, client_Email = 0, "", "" })

	if _, err := (&Tgbot{}).SubmitAddClient(); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("SubmitAddClient with a probe email: err = %v, want the probe guard", err)
	}
}

// TestProbeEmailRefusedOnTunnelClientPaths mirrors the xray guard for the
// AmneziaWG client service.
func TestProbeEmailRefusedOnTunnelClientPaths(t *testing.T) {
	initProbeTestDB(t)
	awg := &AwgService{}
	server, err := awg.GetServer()
	if err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()

	if err := awg.AddClient(&model.TunnelClient{Name: "p", Email: "Probe-AWG"}); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("AddClient(probe): err = %v, want the probe guard", err)
	}
	var n int64
	db.Model(&model.TunnelClient{}).Count(&n)
	if n != 0 {
		t.Errorf("refused AddClient left %d rows", n)
	}

	probe := model.TunnelClient{ServerId: server.Id, UUID: "bbbbbbbb-0000-0000-0000-000000000001",
		Name: "probe-awg", Email: "probe-awg", Enable: true, IPv4Address: "10.66.66.2/32"}
	erin := model.TunnelClient{ServerId: server.Id, UUID: "bbbbbbbb-0000-0000-0000-000000000002",
		Name: "erin", Email: "erin", Enable: true, IPv4Address: "10.66.66.3/32"}
	for _, c := range []*model.TunnelClient{&probe, &erin} {
		if err := db.Create(c).Error; err != nil {
			t.Fatal(err)
		}
	}

	edited := probe
	edited.Comment = "edited"
	if err := awg.UpdateClient(&edited); err == nil {
		t.Error("UpdateClient(probe) succeeded")
	}
	renamed := probe
	renamed.Email = "frank"
	if err := awg.UpdateClientByUUID(probe.UUID, &renamed); err == nil {
		t.Error("UpdateClientByUUID renaming the probe away succeeded")
	}
	erinAsProbe := erin
	erinAsProbe.Email = "probe-awg2"
	if err := awg.UpdateClient(&erinAsProbe); err == nil {
		t.Error("UpdateClient renaming a user into a probe succeeded")
	}
	erinEdited := erin
	erinEdited.Comment = "vip"
	if err := awg.UpdateClient(&erinEdited); err != nil {
		t.Fatalf("UpdateClient(erin): %v", err)
	}
	var stored model.TunnelClient
	db.First(&stored, probe.Id)
	if stored.Email != "probe-awg" || stored.Comment != "" {
		t.Errorf("probe row changed by a refused update: %+v", stored)
	}
}

// TestOnlineSetsSkipProbes: a probe is online whenever mon-server is, and must
// not inflate the online counters of the panel, the bot or the realtime feed.
func TestOnlineSetsSkipProbes(t *testing.T) {
	initProbeTestDB(t)
	db := database.GetDB()
	now := time.Now().UnixMilli()

	vlessInbound(t, 12,
		model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000121", Email: "alice"},
		model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000122", Email: "probe-12"},
		model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000123", Email: "Probe-13"})
	if err := db.Model(&xray.ClientTraffic{}).Where("inbound_id = ?", 12).Update("last_online", now).Error; err != nil {
		t.Fatal(err)
	}
	awg := &AwgService{}
	server, _ := awg.GetServer()
	for i, email := range []string{"bob", "probe-awg"} {
		if err := db.Create(&model.TunnelClient{ServerId: server.Id, UUID: "cccccccc-0000-0000-0000-00000000000" + string(rune('1'+i)),
			Name: email, Email: email, Enable: true, LastOnline: now, IPv4Address: "10.66.66.2/32"}).Error; err != nil {
			t.Fatal(err)
		}
	}

	online := (&InboundService{}).GetOnlineClients()
	want := map[string]bool{"alice": true, "cccccccc-0000-0000-0000-000000000001": true}
	if len(online) != len(want) {
		t.Errorf("GetOnlineClients = %v, want exactly %v", online, want)
	}
	for _, o := range online {
		if !want[o] {
			t.Errorf("GetOnlineClients contains %q", o)
		}
	}
	if tun := awg.GetOnlineClients(); len(tun) != 1 || tun[0] != "cccccccc-0000-0000-0000-000000000001" {
		t.Errorf("AwgService.GetOnlineClients = %v, want only bob's uuid", tun)
	}
}

// TestDelInboundCascadesMonitoringRows: deleting an inbound removes its
// monitoring rows the way it removes client_traffics (§3.6), by the
// (inbound_kind, inbound_id) key — ("awg", 0) for the AmneziaWG server.
func TestDelInboundCascadesMonitoringRows(t *testing.T) {
	initProbeTestDB(t)
	db := database.GetDB()
	s := &InboundService{}

	vlessInbound(t, 5, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000051", Email: "gina"})
	vlessInbound(t, 6, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000061", Email: "hank"})
	if err := db.Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "awg",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: false}).Error; err != nil {
		t.Fatal(err)
	}
	seed := func(kind string, id int) {
		for _, path := range []string{model.MonPathDirect, model.MonPathProxy} {
			db.Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: path, State: model.MonStateUp})
			db.Create(&model.MonEvent{Id: "019254a0-0000-7000-8000-" + kind[:3] + strings.Repeat("0", 7-len(path)) + path + digit(id),
				Ts: 1, ReceivedAt: 1, Kind: model.MonEventKindTarget, MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: path})
			db.Create(&model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: path, BucketStart: 300000, BucketMs: 300000})
			db.Create(&model.MonStatsRollup{MonClientId: "ams-1", InboundKind: kind, InboundId: id, Path: path, StepMs: 3600000})
		}
	}
	seed(model.MonInboundKindXray, 5)
	seed(model.MonInboundKindXray, 6)
	seed(model.MonInboundKindAwg, 0)

	count := func(table, kind string, id int) int64 {
		var n int64
		db.Table(table).Where("inbound_kind = ? AND inbound_id = ?", kind, id).Count(&n)
		return n
	}
	tables := []string{"mon_targets", "mon_events", "mon_stats_current", "mon_stats_rollup"}

	if _, err := s.DelInbound(5); err != nil {
		t.Fatalf("DelInbound(5): %v", err)
	}
	for _, tb := range tables {
		if n := count(tb, model.MonInboundKindXray, 5); n != 0 {
			t.Errorf("%s keeps %d rows of the deleted xray inbound", tb, n)
		}
		if n := count(tb, model.MonInboundKindXray, 6); n != 2 {
			t.Errorf("%s: other xray inbound has %d rows, want 2", tb, n)
		}
		if n := count(tb, model.MonInboundKindAwg, 0); n != 2 {
			t.Errorf("%s: awg has %d rows before its delete, want 2", tb, n)
		}
	}

	if _, err := s.DelInbound(7); err != nil {
		t.Fatalf("DelInbound(awg): %v", err)
	}
	for _, tb := range tables {
		if n := count(tb, model.MonInboundKindAwg, 0); n != 0 {
			t.Errorf("%s keeps %d rows of the deleted awg server", tb, n)
		}
		if n := count(tb, model.MonInboundKindXray, 6); n != 2 {
			t.Errorf("%s: xray inbound 6 lost rows on the awg delete: %d", tb, n)
		}
	}
}

func digit(id int) string { return string(rune('0' + id)) }

func mustInbound(t *testing.T, s *InboundService, id int) *model.Inbound {
	t.Helper()
	ib, err := s.GetInbound(id)
	if err != nil {
		t.Fatalf("GetInbound(%d): %v", id, err)
	}
	return ib
}

func mustClients(t *testing.T, s *InboundService, id int) []model.Client {
	t.Helper()
	clients, err := s.GetClients(mustInbound(t, s, id))
	if err != nil {
		t.Fatalf("GetClients(%d): %v", id, err)
	}
	return clients
}

// TestAddInboundRefusesProbeClients: a new inbound cannot bring a probe
// client of its own (an import, a hand-written settings.clients); probes come
// from EnsureProbeSet only.
func TestAddInboundRefusesProbeClients(t *testing.T) {
	initProbeTestDB(t)
	s := &InboundService{}
	newInbound := func(port int, clients ...model.Client) *model.Inbound {
		settings, _ := json.Marshal(map[string]any{"clients": clients, "decryption": "none"})
		return &model.Inbound{UserId: 1, Port: port, Protocol: model.VLESS, Tag: "inbound-probe-guard",
			Remark: "t", Settings: string(settings), Enable: false, StreamSettings: "{}", Sniffing: "{}"}
	}
	alice := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice", Enable: true}
	for _, email := range []string{"probe-1", "Probe-7", "probe-awg"} {
		probe := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: email, Enable: true}
		if _, _, err := s.AddInbound(newInbound(20001, alice, probe)); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
			t.Errorf("AddInbound with %s: err = %v, want the probe guard", email, err)
		}
	}
	var n int64
	database.GetDB().Model(&model.Inbound{}).Count(&n)
	if n != 0 {
		t.Errorf("a refused inbound was stored: %d inbounds", n)
	}
	if _, _, err := s.AddInbound(newInbound(20002, alice)); err != nil {
		t.Fatalf("AddInbound(alice): %v", err)
	}
}

// TestUpdateInboundRefusesNewProbeClients: editing an inbound keeps the probe
// it already has and may drop it (the next ensure brings it back), but cannot
// add a probe email it did not have — including a case change of its own.
// The refusals come before the edit reaches the database or xray.
func TestUpdateInboundRefusesNewProbeClients(t *testing.T) {
	initProbeTestDB(t)
	s := &InboundService{}
	alice := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice", Enable: false}
	probe := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: "probe-1", Enable: false}
	vlessInbound(t, 1, alice, probe)
	edit := func(clients ...model.Client) *model.Inbound {
		ib := mustInbound(t, s, 1)
		settings, _ := json.Marshal(map[string]any{"clients": clients, "decryption": "none"})
		ib.Settings = string(settings)
		ib.Remark = "edited"
		return ib
	}

	stray := model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000003", Email: "probe-2", Enable: false}
	if _, _, err := s.UpdateInbound(edit(alice, probe, stray)); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("UpdateInbound adding probe-2: err = %v, want the probe guard", err)
	}
	recased := probe
	recased.Email = "Probe-1"
	if _, _, err := s.UpdateInbound(edit(alice, recased)); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("UpdateInbound renaming probe-1 to Probe-1: err = %v, want the probe guard", err)
	}
	aliceAsProbe := alice
	aliceAsProbe.Email = "probe-1000"
	if _, _, err := s.UpdateInbound(edit(aliceAsProbe, probe)); err == nil || !strings.Contains(err.Error(), "monitoring probes") {
		t.Errorf("UpdateInbound renaming a user into a probe: err = %v, want the probe guard", err)
	}
	if got := mustInbound(t, s, 1); got.Remark == "edited" {
		t.Error("a refused edit was stored")
	}

	// The existing probe passes the guard of an ordinary edit, and may be
	// dropped. (The rest of UpdateInbound reaches for the xray API, so the
	// passing cases are asserted on the guard itself.)
	old := mustInbound(t, s, 1)
	if err := s.rejectAddedProbeClients(old, edit(alice, probe)); err != nil {
		t.Errorf("guard on an edit keeping probe-1: %v", err)
	}
	if err := s.rejectAddedProbeClients(old, edit(probe, alice)); err != nil {
		t.Errorf("guard on an edit reordering clients: %v", err)
	}
	if err := s.rejectAddedProbeClients(old, edit(alice)); err != nil {
		t.Errorf("guard on an edit dropping probe-1: %v", err)
	}
}

// TestTunnelResetMovesTheRevision: resetting the AmneziaWG server to defaults
// drops its inbound and probe client, and the monitoring revision moves, so
// mon-server rereads /probe/configs (the AWG target goes PAUSED) and the next
// ensure brings probe-awg back.
func TestTunnelResetMovesTheRevision(t *testing.T) {
	m := newMonitoringTestService(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "AmneziaWG",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AwgService{}).GetServer(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	before, err := m.Revision()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := (&AwgService{}).ResetToDefaults(); err != nil {
		t.Fatalf("ResetToDefaults: %v", err)
	}
	after, err := m.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Errorf("revision did not move on a tunnel reset: %s", after)
	}
	if clients, _ := (&AwgService{}).GetClients(); len(clients) != 0 {
		t.Errorf("awg clients after reset: %+v", clients)
	}
}
