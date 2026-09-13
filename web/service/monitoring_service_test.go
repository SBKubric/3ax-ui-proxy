package service

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// newMonitoringTestDB opens a fresh database, forgets the in-memory
// monitoring state, pretends xray is running (probe accounts can be added
// without a core: the API add fails and schedules a restart) and installs a
// link renderer that records what it was asked for.
func newMonitoringTestDB(t *testing.T) (*MonitoringService, *fakeLinks) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	resetMonRuntime()
	prevXray := monXrayRunning
	monXrayRunning = func() bool { return true }
	prevRender := probeLinkRenderer()
	links := &fakeLinks{}
	RegisterProbeLinkRenderer(links.render)
	t.Cleanup(func() {
		monXrayRunning = prevXray
		RegisterProbeLinkRenderer(prevRender)
		resetMonRuntime()
	})
	return &MonitoringService{}, links
}

type fakeLinks struct {
	host     string
	override bool
	calls    int
}

func (f *fakeLinks) render(host string, override bool) (map[int]string, error) {
	f.host, f.override, f.calls = host, override, f.calls+1
	// One link per enabled inbound with a probe, like the real renderer.
	out := map[int]string{}
	inbounds, err := (&MonitoringService{}).monitoredXrayInbounds()
	if err != nil {
		return nil, err
	}
	for _, ib := range inbounds {
		if !ib.Enable {
			continue
		}
		clients, _ := (&InboundService{}).GetClients(ib)
		if c := findProbeClient(clients, ib.Id); c != nil {
			addr := host
			if override {
				addr = "override"
			}
			out[ib.Id] = "vless://" + c.ID + "@" + addr + "#" + c.Email
		}
	}
	return out, nil
}

func probeEmails(t *testing.T, s *InboundService, id int) []string {
	t.Helper()
	ib, err := s.GetInbound(id)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := s.GetClients(ib)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range clients {
		if IsProbeAccount(c.Email) {
			out = append(out, c.Email)
		}
	}
	return out
}

// TestMonRevisionDeterministicAndSensitive: the revision is a pure function
// of the inputs the contract lists; it moves with enable, override and subId
// and stays put across a rename.
func TestMonRevisionDeterministicAndSensitive(t *testing.T) {
	inbounds := []MonInbound{
		{Kind: "awg", InboundId: 0, Tag: "awg", Remark: "AmneziaWG", Protocol: "awg", Port: 51820, Enable: true},
		{Kind: "xray", InboundId: 12, Tag: "inbound-443", Remark: "Reality main", Protocol: "vless", Port: 443, Enable: true},
	}
	override := MonOverride{Enabled: true, Host: "front.example.net"}
	base := monRevision(override, inbounds, "k3j9d8s7f6g5h4j3")
	if len(base) != 16 {
		t.Fatalf("revision %q is not 16 hex characters", base)
	}
	if again := monRevision(override, inbounds, "k3j9d8s7f6g5h4j3"); again != base {
		t.Fatalf("revision is not deterministic: %s vs %s", base, again)
	}

	renamed := append([]MonInbound(nil), inbounds...)
	renamed[1].Remark, renamed[1].Tag = "renamed", "other-tag"
	if got := monRevision(override, renamed, "k3j9d8s7f6g5h4j3"); got != base {
		t.Errorf("a rename changed the revision")
	}
	disabled := append([]MonInbound(nil), inbounds...)
	disabled[1].Enable = false
	if got := monRevision(override, disabled, "k3j9d8s7f6g5h4j3"); got == base {
		t.Errorf("disabling an inbound did not change the revision")
	}
	if got := monRevision(MonOverride{Enabled: false, Host: ""}, inbounds, "k3j9d8s7f6g5h4j3"); got == base {
		t.Errorf("turning the override off did not change the revision")
	}
	if got := monRevision(MonOverride{Enabled: true, Host: "other.example.net"}, inbounds, "k3j9d8s7f6g5h4j3"); got == base {
		t.Errorf("changing the override host did not change the revision")
	}
	if got := monRevision(override, inbounds, "another-sub-id16"); got == base {
		t.Errorf("changing the probe subId did not change the revision")
	}
}

// TestStateIsSanitisedAndSorted: GET /state lists every monitored inbound,
// disabled ones included, sorted by (kind, inboundId), with the public port,
// a null probe subId before the first ensure, and no settings or keys.
func TestStateIsSanitisedAndSorted(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	seedXrayInbound(t, is, "b-inbound", false, model.Client{ID: "22222222-2222-2222-2222-222222222222", Email: "alice", Enable: true})
	first := seedXrayInbound(t, is, "a-inbound", true)
	if err := database.GetDB().Model(first).Update("public_port", 443).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.Inbound{UserId: 1, Remark: "AmneziaWG", Enable: true, Port: 51820,
		Protocol: model.AmneziaWG, Tag: "inbound-amneziawg", Settings: `{"clients":[]}`}).Error; err != nil {
		t.Fatal(err)
	}
	// An MTProto inbound is not monitored in v1.
	if err := database.GetDB().Create(&model.Inbound{UserId: 1, Remark: "mtproto", Enable: true, Port: 4343,
		Protocol: model.MTProto, Tag: "inbound-4343", Settings: `{}`}).Error; err != nil {
		t.Fatal(err)
	}

	state, err := m.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Contract != 1 || state.ServerTime == 0 || len(state.Revision) != 16 || state.Stale.ThresholdMinutes != 15 {
		t.Fatalf("state header: %+v", state)
	}
	if state.Probe.SubId != nil {
		t.Errorf("probe.subId should be null before the first ensure, got %q", *state.Probe.SubId)
	}
	if state.Override.Enabled || state.Override.Host != "" {
		t.Errorf("override should be off with an empty host: %+v", state.Override)
	}
	if len(state.Inbounds) != 3 {
		t.Fatalf("inbounds: %+v", state.Inbounds)
	}
	if state.Inbounds[0].Kind != "awg" || state.Inbounds[0].InboundId != 0 || state.Inbounds[0].Protocol != "awg" {
		t.Errorf("awg should come first as (awg, 0): %+v", state.Inbounds[0])
	}
	if state.Inbounds[1].InboundId > state.Inbounds[2].InboundId {
		t.Errorf("xray inbounds not sorted by id: %+v", state.Inbounds[1:])
	}
	for _, ib := range state.Inbounds[1:] {
		if ib.Tag == "a-inbound" && (ib.Port != 443 || !ib.Enable) {
			t.Errorf("public port not used: %+v", ib)
		}
		if ib.Tag == "b-inbound" && ib.Enable {
			t.Errorf("disabled inbound reported enabled: %+v", ib)
		}
	}
}

// TestEnsureProbeSetIsIdempotentAndRecreates: the first ensure creates the
// subId and one probe per monitored inbound (disabled ones included); a
// second ensure creates nothing; a deleted probe is recreated with the same
// subId; the registry snapshot replaces the cache and prunes target rows.
func TestEnsureProbeSetIsIdempotentAndRecreates(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	live := seedXrayInbound(t, is, "live", true, model.Client{ID: "22222222-2222-2222-2222-222222222222", Email: "alice", Enable: true, Flow: "xtls-rprx-vision"})
	paused := seedXrayInbound(t, is, "paused", false)
	db := database.GetDB()
	for _, mc := range []string{"ams-1", "gone-1"} {
		if err := db.Create(&model.MonTarget{MonClientId: mc, InboundKind: "xray", InboundId: live.Id, Path: "direct", State: "UP"}).Error; err != nil {
			t.Fatal(err)
		}
	}

	snapshot := []MonClient{{Id: "ams-1", Name: "Amsterdam #1", Region: "NL", State: "ONLINE", LastHeartbeat: 1}}
	res, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("EnsureProbeSet: %v", err)
	}
	if len(res.SubId) != 16 || res.Present != 2 || len(res.Created) != 2 || len(res.Revision) != 16 || res.LastEnsured == 0 {
		t.Fatalf("first ensure: %+v", res)
	}
	if got := probeEmails(t, is, live.Id); len(got) != 1 || got[0] != ProbeEmail("xray", live.Id) {
		t.Errorf("live inbound probes: %v", got)
	}
	if got := probeEmails(t, is, paused.Id); len(got) != 1 {
		t.Errorf("disabled inbound must get a probe too, has %v", got)
	}
	ib, _ := is.GetInbound(live.Id)
	clients, _ := is.GetClients(ib)
	probe := findProbeClient(clients, live.Id)
	if probe.SubID != res.SubId || probe.Flow != "xtls-rprx-vision" || !probe.Enable || probe.Comment != probeComment {
		t.Errorf("probe attributes: %+v", probe)
	}
	if stored, _ := m.settingService.GetMonProbeSubId(); stored != res.SubId {
		t.Errorf("subId not stored: %q", stored)
	}
	if snap := m.Snapshot(); len(snap) != 1 || snap[0].Name != "Amsterdam #1" {
		t.Errorf("snapshot cache: %+v", snap)
	}
	var targets []model.MonTarget
	db.Find(&targets)
	if len(targets) != 1 || targets[0].MonClientId != "ams-1" {
		t.Errorf("targets of mon-clients outside the snapshot must go: %+v", targets)
	}

	again, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("second EnsureProbeSet: %v", err)
	}
	if again.SubId != res.SubId || again.Present != 2 || len(again.Created) != 0 {
		t.Fatalf("second ensure is not idempotent: %+v", again)
	}

	if _, _, err := is.removeProbeClient(ib); err != nil {
		t.Fatal(err)
	}
	if got := probeEmails(t, is, live.Id); len(got) != 0 {
		t.Fatalf("probe still there after removal: %v", got)
	}
	third, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("third EnsureProbeSet: %v", err)
	}
	if third.SubId != res.SubId || len(third.Created) != 1 || third.Created[0].InboundId != live.Id || third.Present != 2 {
		t.Fatalf("deleted probe not recreated: %+v", third)
	}
}

// TestEnsureProbeSetNeedsXrayOnlyToCreate: with xray down an ensure that has
// nothing to create succeeds, one that must add an account answers
// xray_unavailable, and the snapshot is still refreshed.
func TestEnsureProbeSetNeedsXrayOnlyToCreate(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	seedXrayInbound(t, is, "live", true)
	monXrayRunning = func() bool { return false }

	_, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}})
	var monErr *MonError
	if !errors.As(err, &monErr) || monErr.Code != "xray_unavailable" || monErr.Status != 503 {
		t.Fatalf("want 503 xray_unavailable, got %v", err)
	}
	if snap := m.Snapshot(); len(snap) != 1 || snap[0].Id != "ams-1" {
		t.Errorf("snapshot should be refreshed even when creation fails: %+v", snap)
	}

	monXrayRunning = func() bool { return true }
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatalf("EnsureProbeSet with xray: %v", err)
	}
	monXrayRunning = func() bool { return false }
	res, err := m.EnsureProbeSet(nil)
	if err != nil || res.Present != 1 {
		t.Fatalf("ensure with nothing to create should not need xray: %v %+v", err, res)
	}
}

// TestProbeConfigsPaths: probe_not_ensured before the first ensure,
// override_disabled for the proxy path while the override is off, the host
// parameter substituted on the direct path, the override host on the proxy
// path, and disabled inbounds absent from items.
func TestProbeConfigsPaths(t *testing.T) {
	m, links := newMonitoringTestDB(t)
	is := &InboundService{}
	live := seedXrayInbound(t, is, "live", true)
	seedXrayInbound(t, is, "paused", false)

	var monErr *MonError
	if _, err := m.ProbeConfigs("203.0.113.10"); !errors.As(err, &monErr) || monErr.Code != "probe_not_ensured" {
		t.Fatalf("before ensure: want 409 probe_not_ensured, got %v", err)
	}
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ProbeConfigs(""); !errors.As(err, &monErr) || monErr.Code != "override_disabled" || monErr.Status != 409 {
		t.Fatalf("proxy path without override: want 409 override_disabled, got %v", err)
	}

	direct, err := m.ProbeConfigs("203.0.113.10")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if direct.Path != "direct" || links.host != "203.0.113.10" || links.override {
		t.Errorf("direct path: %+v renderer(host=%q override=%v)", direct, links.host, links.override)
	}
	if len(direct.Items) != 1 || direct.Items[0].InboundId != live.Id || !strings.Contains(direct.Items[0].Link, "@203.0.113.10#probe-") {
		t.Errorf("direct items: %+v", direct.Items)
	}
	if len(direct.Revision) != 16 {
		t.Errorf("revision missing: %+v", direct)
	}

	if err := m.settingService.SetProxyOverrideEnable(true); err != nil {
		t.Fatal(err)
	}
	if err := m.settingService.SetProxyOverrideHost("front.example.net"); err != nil {
		t.Fatal(err)
	}
	proxy, err := m.ProbeConfigs("")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if proxy.Path != "proxy" || !links.override || links.host != "" {
		t.Errorf("proxy path: %+v renderer(host=%q override=%v)", proxy, links.host, links.override)
	}
	if len(proxy.Items) != 1 || !strings.Contains(proxy.Items[0].Link, "@override#") {
		t.Errorf("proxy items: %+v", proxy.Items)
	}
}

// TestProbeConfigsAwgConf: the AmneziaWG probe peer is created by ensure and
// its .conf carries the direct host or the override host as Endpoint; a
// disabled tunnel server contributes nothing.
func TestProbeConfigsAwgConf(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{UserId: 1, Remark: "AmneziaWG", Enable: true, Port: 51820,
		Protocol: model.AmneziaWG, Tag: "inbound-amneziawg", Settings: `{"clients":[]}`}).Error; err != nil {
		t.Fatal(err)
	}
	awg := &AwgService{}
	server, err := awg.GetServer()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(server).Updates(map[string]any{"endpoint": "203.0.113.10:51820", "enable": false}).Error; err != nil {
		t.Fatal(err)
	}

	res, err := m.EnsureProbeSet(nil)
	if err != nil {
		t.Fatalf("EnsureProbeSet: %v", err)
	}
	if res.Present != 1 || len(res.Created) != 1 || res.Created[0].Kind != "awg" {
		t.Fatalf("awg probe not created: %+v", res)
	}
	peer, err := m.awgProbeClient()
	if err != nil || peer == nil || peer.Email != "probe-awg" || !peer.Enable {
		t.Fatalf("awg probe peer: %+v %v", peer, err)
	}
	// AddClient switches the server on; switch it off to see the item vanish.
	if err := db.Model(&model.TunnelServer{}).Where("id = ?", server.Id).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	off, err := m.ProbeConfigs("198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if len(off.Items) != 0 {
		t.Errorf("disabled tunnel server must contribute no item: %+v", off.Items)
	}

	if err := db.Model(&model.TunnelServer{}).Where("id = ?", server.Id).Update("enable", true).Error; err != nil {
		t.Fatal(err)
	}
	direct, err := m.ProbeConfigs("198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Items) != 1 || direct.Items[0].Kind != "awg" || direct.Items[0].Filename != "probe-awg" {
		t.Fatalf("direct awg item: %+v", direct.Items)
	}
	if !strings.Contains(direct.Items[0].Conf, "Endpoint = 198.51.100.7:51820") || !strings.Contains(direct.Items[0].Conf, "PrivateKey = "+peer.PrivateKey) {
		t.Errorf("direct conf:\n%s", direct.Items[0].Conf)
	}
	if err := m.settingService.SetProxyOverrideEnable(true); err != nil {
		t.Fatal(err)
	}
	if err := m.settingService.SetProxyOverrideHost("front.example.net"); err != nil {
		t.Fatal(err)
	}
	proxy, err := m.ProbeConfigs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(proxy.Items) != 1 || !strings.Contains(proxy.Items[0].Conf, "Endpoint = front.example.net:51820") {
		t.Errorf("proxy conf:\n%+v", proxy.Items)
	}
}

// TestDeleteProbeSetForgetsEverything: every probe account goes, the subId,
// the ensure time, the snapshot and the target rows are cleared, and a later
// ensure starts a fresh set with a new subId.
func TestDeleteProbeSetForgetsEverything(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	is := &InboundService{}
	live := seedXrayInbound(t, is, "live", true, model.Client{ID: "22222222-2222-2222-2222-222222222222", Email: "alice", Enable: true})
	db := database.GetDB()
	if err := db.Create(&model.Inbound{UserId: 1, Remark: "AmneziaWG", Enable: true, Port: 51820,
		Protocol: model.AmneziaWG, Tag: "inbound-amneziawg", Settings: `{"clients":[]}`}).Error; err != nil {
		t.Fatal(err)
	}
	first, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: "xray", InboundId: live.Id, Path: "direct", State: "UP"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteProbeSet(); err != nil {
		t.Fatalf("DeleteProbeSet: %v", err)
	}
	if got := probeEmails(t, is, live.Id); len(got) != 0 {
		t.Errorf("xray probe still present: %v", got)
	}
	ib, _ := is.GetInbound(live.Id)
	clients, _ := is.GetClients(ib)
	if len(clients) != 1 || clients[0].Email != "alice" {
		t.Errorf("users must survive: %+v", clients)
	}
	if peer, _ := m.awgProbeClient(); peer != nil {
		t.Errorf("awg probe still present: %+v", peer)
	}
	if v, _ := m.settingService.GetMonProbeSubId(); v != "" {
		t.Errorf("subId not cleared: %q", v)
	}
	if v, _ := m.settingService.GetMonProbeLastEnsured(); v != 0 {
		t.Errorf("lastEnsured not cleared: %d", v)
	}
	if snap := m.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot not cleared: %+v", snap)
	}
	var n int64
	db.Model(&model.MonTarget{}).Count(&n)
	if n != 0 {
		t.Errorf("target rows survive the snapshot reset: %d", n)
	}
	if count, _ := m.ProbeAccountCount(); count != 0 {
		t.Errorf("ProbeAccountCount = %d after delete", count)
	}

	second, err := m.EnsureProbeSet(nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.SubId == first.SubId || second.Present != 2 {
		t.Errorf("fresh set after delete: %+v (old subId %s)", second, first.SubId)
	}
}

// TestLastContactMemoryFirst: TouchLastContact is always visible in memory
// and reaches the setting at most every ten seconds; the memory copy wins over
// a rolled-back setting.
func TestLastContactMemoryFirst(t *testing.T) {
	m, _ := newMonitoringTestDB(t)
	if got := m.LastContact(); got != 0 {
		t.Fatalf("LastContact before any request = %d", got)
	}
	t0 := int64(1_757_721_600_000)
	m.TouchLastContact(msTime(t0))
	m.TouchLastContact(msTime(t0 + 5_000))
	if got := m.LastContact(); got != t0+5_000 {
		t.Errorf("memory LastContact = %d", got)
	}
	if stored, _ := m.settingService.GetMonLastContact(); stored != t0 {
		t.Errorf("setting should hold the first touch only (%d), got %d", t0, stored)
	}
	m.TouchLastContact(msTime(t0 + 10_000))
	if stored, _ := m.settingService.GetMonLastContact(); stored != t0+10_000 {
		t.Errorf("setting should be refreshed after ten seconds, got %d", stored)
	}
	if err := m.settingService.SetMonLastContact(1); err != nil { // a stale settings form
		t.Fatal(err)
	}
	if got := m.LastContact(); got != t0+10_000 {
		t.Errorf("memory must win over the setting, got %d", got)
	}
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms) }
