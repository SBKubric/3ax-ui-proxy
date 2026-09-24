package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// fakeLinks stands in for sub.SubService: it records what address and
// override mode the service asked for.
type fakeLinks struct{}

func (fakeLinks) ProbeLink(inbound *model.Inbound, email, address string, useOverride bool) string {
	mode := "direct"
	if useOverride {
		mode = "override"
	}
	return string(inbound.Protocol) + "://" + email + "@" + address + "/" + mode
}

func newMonitoringTestService(t *testing.T) *MonitoringService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		database.CloseDB()
		monRegistry.Lock()
		monRegistry.loaded, monRegistry.clients = false, nil
		monRegistry.Unlock()
	})
	monRegistry.Lock()
	monRegistry.loaded, monRegistry.clients = false, nil
	monRegistry.Unlock()
	return &MonitoringService{Links: fakeLinks{}}
}

// monInbound stores an inbound with the given clients (and their traffic rows).
func monInbound(t *testing.T, id int, protocol model.Protocol, enable bool, clients ...model.Client) *model.Inbound {
	t.Helper()
	settings := map[string]any{"clients": clients}
	if protocol == model.VLESS {
		settings["decryption"] = "none"
	}
	if protocol == model.Shadowsocks {
		settings["method"] = "2022-blake3-aes-128-gcm"
	}
	raw, _ := json.Marshal(settings)
	ib := &model.Inbound{Id: id, Port: 10000 + id, Protocol: protocol, Tag: "inbound-" + ProbeXrayEmail(id), Remark: "r",
		Settings: string(raw), Enable: enable, StreamSettings: "{}", Sniffing: "{}"}
	db := database.GetDB()
	if err := db.Create(ib).Error; err != nil {
		t.Fatalf("create inbound %d: %v", id, err)
	}
	for _, c := range clients {
		if err := db.Create(&xray.ClientTraffic{InboundId: id, Email: c.Email, Enable: true}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return ib
}

func setSetting(t *testing.T, key, value string) {
	t.Helper()
	if err := (&SettingService{}).setString(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

func probeEmails(t *testing.T, id int) map[string]model.Client {
	t.Helper()
	s := &InboundService{}
	clients := mustClients(t, s, id)
	out := map[string]model.Client{}
	for _, c := range clients {
		out[c.Email] = c
	}
	return out
}

// TestStateIsSanitisedAndSorted: GET /state lists client-facing xray
// inbounds (disabled included) and the AmneziaWG server as ("awg", 0), sorted
// by (kind, inboundId), with the public port and nothing secret.
func TestStateIsSanitisedAndSorted(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 12, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	monInbound(t, 3, model.Trojan, false, model.Client{Password: "pw", Email: "bob"})
	monInbound(t, 4, model.MTProto, true)
	db := database.GetDB()
	if err := db.Model(&model.Inbound{}).Where("id = ?", 12).Update("public_port", 443).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "AmneziaWG",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}

	st, err := m.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Contract != 1 || st.Probe.SubId != nil || st.Stale.ThresholdMinutes != 15 || st.Override.Enabled || len(st.Revision) != 16 {
		t.Errorf("state header: %+v", st)
	}
	want := []MonInbound{
		{Kind: "awg", InboundId: 0, Tag: "awg", Remark: "AmneziaWG", Protocol: "awg", Port: 51820, Enable: true},
		{Kind: "xray", InboundId: 3, Tag: "inbound-probe-3", Remark: "r", Protocol: "trojan", Port: 10003, Enable: false},
		{Kind: "xray", InboundId: 12, Tag: "inbound-probe-12", Remark: "r", Protocol: "vless", Port: 443, Enable: true},
	}
	if len(st.Inbounds) != len(want) {
		t.Fatalf("inbounds = %+v, want %+v", st.Inbounds, want)
	}
	for i := range want {
		if st.Inbounds[i] != want[i] {
			t.Errorf("inbounds[%d] = %+v, want %+v", i, st.Inbounds[i], want[i])
		}
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"aaaaaaaa-0000", "\"pw\"", "settings", "streamSettings"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("state leaks %q: %s", secret, raw)
		}
	}
}

// TestRevisionTracksTargets: the revision is stable across calls and moves
// with enable, the override and the probe subId — not with a remark.
func TestRevisionTracksTargets(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	db := database.GetDB()
	rev := monRevision(t, m)

	base := rev()
	if base != rev() {
		t.Fatal("revision differs between two identical calls")
	}
	db.Model(&model.Inbound{}).Where("id = ?", 1).Update("remark", "renamed")
	if rev() != base {
		t.Error("a remark change moved the revision")
	}
	db.Model(&model.Inbound{}).Where("id = ?", 1).Update("enable", false)
	afterDisable := rev()
	if afterDisable == base {
		t.Error("disabling an inbound left the revision unchanged")
	}
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	afterOverride := rev()
	if afterOverride == afterDisable {
		t.Error("turning the override on left the revision unchanged")
	}
	setSetting(t, "monProbeSubId", "k3j9d8s7f6g5h4j3")
	if rev() == afterOverride {
		t.Error("a new probe subId left the revision unchanged")
	}
}

// monRevision returns a getter of m's current revision that fails the test on
// an error.
func monRevision(t *testing.T, m *MonitoringService) func() string {
	return func() string {
		t.Helper()
		r, err := m.Revision()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

// probeRealityStream is a Reality streamSettings with the fields a probe link
// carries; edit applies a change to its realitySettings before encoding.
func probeRealityStream(edit func(reality map[string]any)) string {
	reality := map[string]any{
		"show": false, "xver": 0, "target": "www.microsoft.com:443",
		"serverNames": []string{"www.microsoft.com"}, "privateKey": "priv-1", "shortIds": []string{"ab12"},
		"settings": map[string]any{"publicKey": "pub-1", "fingerprint": "chrome", "spiderX": "/"},
	}
	if edit != nil {
		edit(reality)
	}
	raw, _ := json.Marshal(map[string]any{"network": "tcp", "security": "reality", "realitySettings": reality})
	return string(raw)
}

// TestRevision_CoversRealityMaterial (#117): the revision hashed only the
// target fields, so a Reality edit (serverNames, target, keys, shortIds) kept
// it, mon-server never reread /probe/configs, and mon-clients probed with the
// stale serverName until something else changed. Every probe-relevant stream
// field moves it now; a remark, a user of the inbound and the key order of the
// stored JSON do not.
func TestRevision_CoversRealityMaterial(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	db := database.GetDB()
	setStream := func(stream string) {
		t.Helper()
		if err := db.Model(&model.Inbound{}).Where("id = ?", 1).Update("stream_settings", stream).Error; err != nil {
			t.Fatal(err)
		}
	}
	setStream(probeRealityStream(nil))
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	rev := monRevision(t, m)
	base := rev()

	for name, edit := range map[string]func(map[string]any){
		"serverNames": func(r map[string]any) { r["serverNames"] = []string{"dl.google.com"} },
		"target":      func(r map[string]any) { r["target"] = "dl.google.com:443" },
		"privateKey":  func(r map[string]any) { r["privateKey"] = "priv-2" },
		"publicKey":   func(r map[string]any) { r["settings"].(map[string]any)["publicKey"] = "pub-2" },
		"shortIds":    func(r map[string]any) { r["shortIds"] = []string{"cd34"} },
		"fingerprint": func(r map[string]any) { r["settings"].(map[string]any)["fingerprint"] = "firefox" },
	} {
		setStream(probeRealityStream(edit))
		if rev() == base {
			t.Errorf("changing Reality %s left the revision unchanged", name)
		}
	}
	setStream(probeRealityStream(nil))
	if rev() != base {
		t.Fatal("restoring the stream did not restore the revision")
	}
	setStream(`{"network":"ws","security":"tls"}`)
	if rev() == base {
		t.Error("changing the transport and security left the revision unchanged")
	}
	setStream(probeRealityStream(nil))

	// Not material: the remark, the externalProxy probe links drop, an
	// ordinary user, the key order of the stored JSON.
	db.Model(&model.Inbound{}).Where("id = ?", 1).Update("remark", "renamed")
	if rev() != base {
		t.Error("a remark rename moved the revision")
	}
	var stream map[string]any
	_ = json.Unmarshal([]byte(probeRealityStream(nil)), &stream)
	stream["externalProxy"] = []any{map[string]any{"dest": "cdn.example.net", "port": 443}}
	withProxy, _ := json.Marshal(stream)
	setStream(string(withProxy))
	if rev() != base {
		t.Error("an externalProxy entry moved the revision")
	}
	setStream(`{"security":"reality","realitySettings":{"shortIds":["ab12"],"settings":{"spiderX":"/","fingerprint":"chrome","publicKey":"pub-1"},` +
		`"privateKey":"priv-1","serverNames":["www.microsoft.com"],"target":"www.microsoft.com:443","xver":0,"show":false},"network":"tcp"}`)
	if rev() != base {
		t.Error("the same stream stored with another key order moved the revision")
	}
	if _, err := (&InboundService{}).AddInboundClient(clientsPayload(1, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: "bob"})); err != nil {
		t.Fatal(err)
	}
	if rev() != base {
		t.Error("adding a user moved the revision")
	}

	// The probe client itself is material: a recreated probe has a new uuid.
	if _, err := (&InboundService{}).DelInboundClientByEmail(1, "probe-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	afterProbe := rev()
	if afterProbe == base {
		t.Error("recreating the probe client left the revision unchanged")
	}

	// A restart: fresh services, the registry cache dropped. Same revision.
	monRegistry.Lock()
	monRegistry.loaded, monRegistry.clients = false, nil
	monRegistry.Unlock()
	if again, _ := (&MonitoringService{Links: fakeLinks{}}).Revision(); again != afterProbe {
		t.Errorf("revision after a restart = %s, want %s", again, afterProbe)
	}
}

// TestRevision_PinsTheCanonicalForm writes the canonical object of
// monitoring-contract.md §4.2 out by hand for one xray inbound, so a change
// to the form — a field added, dropped or renamed — fails here and has to be
// made in the contract too.
func TestRevision_PinsTheCanonicalForm(t *testing.T) {
	m := newMonitoringTestService(t)
	db := database.GetDB()
	settings := `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000001","email":"alice"},` +
		`{"id":"aaaaaaaa-0000-0000-0000-000000000002","email":"probe-12","flow":"xtls-rprx-vision"}],"decryption":"none"}`
	if err := db.Create(&model.Inbound{Id: 12, Port: 8443, Listen: "", Protocol: model.VLESS, Tag: "inbound-8443", Remark: "Reality main",
		Settings: settings, StreamSettings: `{"network":"tcp","security":"reality","externalProxy":[],"realitySettings":{"serverNames":["a.example"]}}`,
		Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	db.Model(&model.Inbound{}).Where("id = ?", 12).Update("public_port", 443)
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	setSetting(t, "monProbeSubId", "k3j9d8s7f6g5h4j3")

	canonical := `{"hiddifyCompat":false,"inbounds":[{"enable":true,"inboundId":12,"kind":"xray","listen":"","port":443,"protocol":"vless",` +
		`"settings":{"clients":[{"email":"probe-12","flow":"xtls-rprx-vision","id":"aaaaaaaa-0000-0000-0000-000000000002"}],"decryption":"none"},` +
		`"stream":{"network":"tcp","realitySettings":{"serverNames":["a.example"]},"security":"reality"}}],` +
		`"override":{"enabled":true,"host":"front.example.net"},"probeSubId":"k3j9d8s7f6g5h4j3"}`
	sum := sha256.Sum256([]byte(canonical))
	if got, want := monRevision(t, m)(), hex.EncodeToString(sum[:])[:16]; got != want {
		t.Errorf("revision = %s, want %s (the hash of %s)", got, want, canonical)
	}
}

// TestRevision_CoversTunnelMaterial (#117): the AmneziaWG server's public
// parameters and the probe peer's own keys are in the .conf mon-clients dial
// with, so each moves the revision; the remark does not.
func TestRevision_CoversTunnelMaterial(t *testing.T) {
	m := newMonitoringTestService(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "AmneziaWG",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	server, err := (&AwgService{}).GetServer()
	if err != nil {
		t.Fatal(err)
	}
	rev := monRevision(t, m)
	noPeer := rev()
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	base := rev()
	if base == noPeer {
		t.Error("creating the probe peer left the revision unchanged")
	}

	for _, tc := range []struct {
		name, table, column string
		value               any
	}{
		{"server listen port", "tunnel_servers", "listen_port", server.ListenPort + 1},
		{"server public key", "tunnel_servers", "public_key", "c2VydmVyLXB1YmxpYy1rZXktcm90YXRlZC0wMDAwMDA="},
		{"server MTU", "tunnel_servers", "mtu", 1280},
		{"obfuscation H1", "tunnel_servers", "h1", "123456"},
		{"peer private key", "tunnel_clients", "private_key", "cGVlci1wcml2YXRlLWtleS1yb3RhdGVkLTAwMDAwMDA="},
		{"peer preshared key", "tunnel_clients", "preshared_key", "cGVlci1wc2stcm90YXRlZC0wMDAwMDAwMDAwMDAwMDA="},
		{"peer address", "tunnel_clients", "ipv4_address", "10.66.66.9/32"},
	} {
		var old any
		if err := db.Table(tc.table).Select(tc.column).Limit(1).Row().Scan(&old); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := db.Table(tc.table).Where("1 = 1").Update(tc.column, tc.value).Error; err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rev() == base {
			t.Errorf("changing the %s left the revision unchanged", tc.name)
		}
		db.Table(tc.table).Where("1 = 1").Update(tc.column, old)
		if rev() != base {
			t.Fatalf("restoring the %s did not restore the revision", tc.name)
		}
	}

	db.Model(&model.Inbound{}).Where("id = ?", 7).Update("remark", "renamed")
	if rev() != base {
		t.Error("renaming the AmneziaWG inbound moved the revision")
	}
}

// TestEnsureProbeSetIsIdempotent: the first ensure creates one probe per
// client-facing inbound (disabled ones too); a second creates nothing; a
// deleted probe is recreated with a new identity under the same subId.
func TestEnsureProbeSetIsIdempotent(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice", Flow: "xtls-rprx-vision"})
	monInbound(t, 2, model.Trojan, false, model.Client{Password: "pw", Email: "bob"})
	monInbound(t, 3, model.Shadowsocks, true, model.Client{Password: "cGFzcw==", Email: "carol"})
	monInbound(t, 4, model.MTProto, true)

	snapshot := []MonClient{{Id: "ams-1", Name: "Amsterdam", State: "ONLINE", LastHeartbeat: 1}}
	first, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if len(first.SubId) != 16 || first.Present != 3 || len(first.Created) != 3 || first.LastEnsured == 0 || len(first.Revision) != 16 {
		t.Errorf("first ensure: %+v", first)
	}
	if got, _ := (&SettingService{}).GetMonProbeSubId(); got != first.SubId {
		t.Errorf("monProbeSubId = %q, want %q", got, first.SubId)
	}
	probe1 := probeEmails(t, 1)["probe-1"]
	if probe1.ID == "" || probe1.SubID != first.SubId || probe1.Flow != "xtls-rprx-vision" || !probe1.Enable ||
		probe1.Comment != ProbeComment || probe1.TotalGB != 0 || probe1.LimitIP != 0 {
		t.Errorf("vless probe: %+v", probe1)
	}
	if p := probeEmails(t, 2)["probe-2"]; p.Password == "" || p.ID != "" {
		t.Errorf("trojan probe: %+v", p)
	}
	if p := probeEmails(t, 3)["probe-3"]; len(p.Password) != 24 { // 16 bytes base64 for aes-128
		t.Errorf("shadowsocks-2022 probe password %q, want a 16-byte base64 key", p.Password)
	}
	var traffic int64
	database.GetDB().Model(&xray.ClientTraffic{}).Where("email LIKE 'probe-%'").Count(&traffic)
	if traffic != 3 {
		t.Errorf("probe traffic rows = %d, want 3", traffic)
	}

	second, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(second.Created) != 0 || second.Present != 3 || second.SubId != first.SubId {
		t.Errorf("second ensure: %+v", second)
	}
	if again := probeEmails(t, 1)["probe-1"]; again.ID != probe1.ID {
		t.Error("second ensure replaced an existing probe")
	}

	// Deleting a probe through the ordinary path is allowed; ensure recreates it.
	if _, err := (&InboundService{}).DelInboundClientByEmail(1, "probe-1"); err != nil {
		t.Fatalf("delete probe: %v", err)
	}
	third, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("third ensure: %v", err)
	}
	if len(third.Created) != 1 || third.Created[0] != (MonInboundRef{"xray", 1}) {
		t.Errorf("third ensure created %+v, want the deleted probe only", third.Created)
	}
	if recreated := probeEmails(t, 1)["probe-1"]; recreated.ID == probe1.ID || recreated.SubID != first.SubId {
		t.Errorf("recreated probe: %+v (old id %s)", recreated, probe1.ID)
	}
}

// TestEnsureReplacesSnapshotAndDropsStrayTargets: the body is the whole
// registry; mon-clients that left take their mon_targets with them, and the
// snapshot is persisted for a restart.
func TestEnsureReplacesSnapshotAndDropsStrayTargets(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	db := database.GetDB()
	for _, id := range []string{"ams-1", "msk-1", "old-1"} {
		if err := db.Create(&model.MonTarget{MonClientId: id, InboundKind: "xray", InboundId: 1, Path: "proxy", State: "UP"}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}, {Id: "msk-1", State: "OFFLINE"}}); err != nil {
		t.Fatal(err)
	}
	var left []string
	db.Model(&model.MonTarget{}).Order("mon_client_id").Pluck("mon_client_id", &left)
	if strings.Join(left, ",") != "ams-1,msk-1" {
		t.Errorf("targets after ensure = %v, want ams-1 and msk-1", left)
	}

	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}}); err != nil {
		t.Fatal(err)
	}
	db.Model(&model.MonTarget{}).Pluck("mon_client_id", &left)
	if strings.Join(left, ",") != "ams-1" {
		t.Errorf("targets after shrinking the registry = %v, want ams-1", left)
	}

	// A fresh service (a restart) reads the snapshot back from settings.
	monRegistry.Lock()
	monRegistry.loaded, monRegistry.clients = false, nil
	monRegistry.Unlock()
	snap := (&MonitoringService{}).RegistrySnapshot()
	if len(snap) != 1 || snap[0].Id != "ams-1" || snap[0].State != "ONLINE" {
		t.Errorf("RegistrySnapshot after reload = %+v", snap)
	}
}

// TestProbeConfigsPaths: 409 before the first ensure, 409 for the proxy path
// with the override off, the given host on the direct path, the override on
// the proxy path, and no item for a disabled inbound.
func TestProbeConfigsPaths(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	monInbound(t, 2, model.VMESS, false, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: "bob"})

	var monErr *MonError
	if _, err := m.ProbeConfigs("203.0.113.10", "", ""); !errors.As(err, &monErr) || monErr != ErrProbeNotEnsured {
		t.Errorf("before ensure: err = %v, want probe_not_ensured", err)
	}
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ProbeConfigs("", "", ""); !errors.As(err, &monErr) || monErr != ErrOverrideDisabled {
		t.Errorf("proxy path with the override off: err = %v, want override_disabled", err)
	}

	direct, err := m.ProbeConfigs("203.0.113.10", "", "")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if direct.Path != "direct" || len(direct.Items) != 1 || direct.Items[0].Kind != "xray" || direct.Items[0].InboundId != 1 ||
		direct.Items[0].Link != "vless://probe-1@203.0.113.10/direct" || len(direct.Revision) != 16 {
		t.Errorf("direct configs: %+v", direct)
	}

	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	proxy, err := m.ProbeConfigs("", "", "")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if proxy.Path != "proxy" || len(proxy.Items) != 1 || proxy.Items[0].Link != "vless://probe-1@/override" {
		t.Errorf("proxy configs: %+v", proxy)
	}

	m.Links = nil
	SetProbeLinkRenderer(nil)
	if _, err := m.ProbeConfigs("203.0.113.10", "", ""); !errors.As(err, &monErr) || monErr != ErrLinksNotWired {
		t.Errorf("without a renderer: err = %v, want the wiring error", err)
	}
}

// TestDeleteProbeSetForgetsEverything: every probe goes, including one that
// was the only client of its inbound, and the panel forgets subId, last
// ensure and snapshot.
func TestDeleteProbeSetForgetsEverything(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	monInbound(t, 2, model.VLESS, false) // no users: the probe will be alone
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := probeEmails(t, 2)["probe-2"]; !ok {
		t.Fatal("probe-2 was not created")
	}

	if err := m.DeleteProbeSet(); err != nil {
		t.Fatalf("DeleteProbeSet: %v", err)
	}
	if c := probeEmails(t, 1); len(c) != 1 || c["alice"].Email != "alice" {
		t.Errorf("inbound 1 clients after delete: %+v", c)
	}
	if c := probeEmails(t, 2); len(c) != 0 {
		t.Errorf("inbound 2 clients after delete: %+v", c)
	}
	var traffic int64
	database.GetDB().Model(&xray.ClientTraffic{}).Where("email LIKE 'probe-%'").Count(&traffic)
	if traffic != 0 {
		t.Errorf("probe traffic rows left: %d", traffic)
	}
	st, _ := m.State()
	if st.Probe.SubId != nil || st.Probe.LastEnsured != 0 || len(m.RegistrySnapshot()) != 0 {
		t.Errorf("state after delete: %+v, snapshot %v", st.Probe, m.RegistrySnapshot())
	}
}

// TestEnsureCoversTheTunnelServer: the AmneziaWG server gets a probe client
// too, and ProbeConfigs renders its .conf with the chosen endpoint host.
func TestEnsureCoversTheTunnelServer(t *testing.T) {
	m := newMonitoringTestService(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "AmneziaWG",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AwgService{}).GetServer(); err != nil {
		t.Fatal(err)
	}

	res, err := m.EnsureProbeSet(nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Present != 1 || len(res.Created) != 1 || res.Created[0] != (MonInboundRef{"awg", 0}) {
		t.Errorf("ensure with an AWG server: %+v", res)
	}
	clients, _ := (&AwgService{}).GetClients()
	if len(clients) != 1 || clients[0].Email != "probe-awg" || !clients[0].Enable || clients[0].PublicKey == "" {
		t.Errorf("awg probe client: %+v", clients)
	}
	if again, _ := m.EnsureProbeSet(nil); len(again.Created) != 0 {
		t.Errorf("second ensure recreated the awg probe: %+v", again)
	}

	direct, err := m.ProbeConfigs("203.0.113.10", "", "")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if len(direct.Items) != 1 || direct.Items[0].Kind != "awg" || direct.Items[0].Filename != "probe-awg" ||
		!strings.Contains(direct.Items[0].Conf, "Endpoint = 203.0.113.10:") {
		t.Errorf("awg direct item: %+v", direct.Items)
	}

	if err := m.DeleteProbeSet(); err != nil {
		t.Fatal(err)
	}
	clients, _ = (&AwgService{}).GetClients()
	if len(clients) != 0 {
		t.Errorf("awg clients after delete: %+v", clients)
	}
}

// multiLinks renders a multi-link inbound the way /sub does with several
// external proxies: one link per line.
type multiLinks struct{}

func (multiLinks) ProbeLink(inbound *model.Inbound, email, address string, useOverride bool) string {
	return "vless://" + email + "@first.example.net\nvless://" + email + "@second.example.net"
}

// TestProbeConfigsTakesTheFirstLink: a renderer that hands back several lines
// still yields one link per inbound, the first (monitoring-panel.md §4.3).
func TestProbeConfigsTakesTheFirstLink(t *testing.T) {
	m := newMonitoringTestService(t)
	m.Links = multiLinks{}
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	direct, err := m.ProbeConfigs("203.0.113.10", "", "")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if len(direct.Items) != 1 || direct.Items[0].Link != "vless://probe-1@first.example.net" {
		t.Errorf("items: %+v", direct.Items)
	}
}

// TestProbeConfigsRefusesHops: until per-hop probing (proxy-chain §6.1)
// every named hop is unknown, whether asked by ?hop= or its synonym ?edge=,
// and never falls back to the proxy path. Two different names are refused
// too; the same name twice is one request.
func TestProbeConfigsRefusesHops(t *testing.T) {
	m := newMonitoringTestService(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, hop, edge, code string
	}{
		{"", "ams-1", "", "unknown_hop"},
		{"", "", "ams-1", "unknown_edge"},
		{"", "ams-1", "ams-1", "unknown_hop"},
		{"", "ams-1", "core-1", "unknown_hop"},
		{"203.0.113.10", "ams-1", "", "unknown_hop"},
		{"", " ams-1 ", "", "unknown_hop"},
	} {
		_, err := m.ProbeConfigs(tc.host, tc.hop, tc.edge)
		var monErr *MonError
		if !errors.As(err, &monErr) || monErr.Status != 409 || monErr.Code != tc.code {
			t.Errorf("host=%q hop=%q edge=%q: err = %v, want 409 %s", tc.host, tc.hop, tc.edge, err, tc.code)
		}
	}
	// Blank parameters are absent: the proxy path as before.
	if res, err := m.ProbeConfigs("", " ", ""); err != nil || res.Path != "proxy" {
		t.Errorf("blank hop: %+v, %v", res, err)
	}
}
