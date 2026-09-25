package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/xray"
	"gorm.io/gorm"
)

// fakeLinks stands in for sub.SubService: it records what address and
// override mode the service asked for.
type fakeLinks struct{}

func (fakeLinks) ProbeLink(inbound *model.Inbound, email, address, via string) string {
	if via != "" {
		return string(inbound.Protocol) + "://" + email + "@" + via + "/via"
	}
	return string(inbound.Protocol) + "://" + email + "@" + address + "/direct"
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
	if st.Contract != 3 || st.Probe.SubId != nil || st.Stale.ThresholdMinutes != 15 || st.Override.Enabled || len(st.Revision) != 16 {
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
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}}); err != nil {
		t.Fatal(err)
	}
	base := rev()
	if base == noPeer {
		t.Error("creating the probe peers left the revision unchanged")
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
		// One row: the server, or the direct peer of ams-1.
		row := func() *gorm.DB {
			if tc.table == "tunnel_clients" {
				return db.Table(tc.table).Where("email = ?", "probe-awg-ams-1-direct")
			}
			return db.Table(tc.table).Where("1 = 1")
		}
		var old any
		if err := row().Select(tc.column).Row().Scan(&old); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := row().Update(tc.column, tc.value).Error; err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rev() == base {
			t.Errorf("changing the %s left the revision unchanged", tc.name)
		}
		row().Update(tc.column, old)
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
	if len(third.Created) != 1 || third.Created[0] != (MonProbeRef{Kind: "xray", InboundId: 1}) {
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
	if proxy.Path != "proxy" || len(proxy.Items) != 1 || proxy.Items[0].Link != "vless://probe-1@front.example.net/via" {
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

// awgTestServer stores an enabled AmneziaWG inbound and its server row.
func awgTestServer(t *testing.T) {
	t.Helper()
	if err := database.GetDB().Create(&model.Inbound{Id: 7, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "AmneziaWG",
		Settings: "{}", StreamSettings: "{}", Sniffing: "{}", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&AwgService{}).GetServer(); err != nil {
		t.Fatal(err)
	}
}

// awgProbePeers maps the AmneziaWG probe peers by email.
func awgProbePeers(t *testing.T) map[string]model.TunnelClient {
	t.Helper()
	clients, err := (&AwgService{}).GetClients()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.TunnelClient{}
	for _, c := range clients {
		if IsProbeAccount(c.Email) {
			out[c.Email] = c
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// TestEnsureReconcilesTunnelPeers (#120): ensure gives every mon-client of
// the snapshot, whatever its state, its own AmneziaWG probe peer on each path
// the panel serves, keeps the ones it already has, and deletes the peers of
// mon-clients that left. An empty snapshot leaves no peer.
func TestEnsureReconcilesTunnelPeers(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	db := database.GetDB()
	if err := db.Create(&model.TunnelClient{ServerId: 1, UUID: "bbbbbbbb-0000-0000-0000-000000000001", Name: "erin", Email: "erin",
		Enable: true, IPv4Address: "10.66.66.40/32"}).Error; err != nil {
		t.Fatal(err)
	}

	res, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}, {Id: "msk-1", State: "NEVER"}})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got, want := sortedKeys(awgProbePeers(t)), "probe-awg-ams-1-direct,probe-awg-ams-1-proxy,probe-awg-msk-1-direct,probe-awg-msk-1-proxy"; got != want {
		t.Fatalf("peers = %s, want %s", got, want)
	}
	if res.Present != 4 || len(res.Created) != 4 || len(res.Unallocated) != 0 ||
		res.Created[0] != (MonProbeRef{Kind: "awg", InboundId: 0, MonClientId: "ams-1", Path: "direct"}) {
		t.Errorf("first ensure: %+v", res)
	}
	peers := awgProbePeers(t)
	addresses := map[string]bool{}
	for _, p := range peers {
		if p.PublicKey == "" || !p.Enable || p.Comment != ProbeComment || addresses[p.IPv4Address] {
			t.Errorf("peer %+v: want its own keys and address", p)
		}
		addresses[p.IPv4Address] = true
	}

	again, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "ONLINE"}, {Id: "msk-1", State: "NEVER"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Created) != 0 || again.Present != 4 || awgProbePeers(t)["probe-awg-ams-1-direct"].PublicKey != peers["probe-awg-ams-1-direct"].PublicKey {
		t.Errorf("second ensure replaced peers: %+v", again)
	}

	// msk-1 leaves, ber-1 joins: msk-1's peers go, ber-1 gets a pair.
	res, err = m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "OFFLINE"}, {Id: "ber-1", State: "NEVER"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(awgProbePeers(t)), "probe-awg-ams-1-direct,probe-awg-ams-1-proxy,probe-awg-ber-1-direct,probe-awg-ber-1-proxy"; got != want {
		t.Errorf("peers after the registry changed = %s, want %s", got, want)
	}
	if len(res.Created) != 2 || res.Created[0].MonClientId != "ber-1" || res.Present != 4 {
		t.Errorf("ensure after the registry changed: %+v", res)
	}

	if _, err := m.EnsureProbeSet(nil); err != nil {
		t.Fatal(err)
	}
	if peers := awgProbePeers(t); len(peers) != 0 {
		t.Errorf("peers after an empty snapshot: %s", sortedKeys(peers))
	}
	var users int64
	db.Model(&model.TunnelClient{}).Where("email = ?", "erin").Count(&users)
	if users != 1 {
		t.Error("reconciling the probe peers touched a user")
	}
}

// TestEnsureDeletesTheSharedTunnelProbe (#120): the one shared probe-awg of
// contract v1 is deleted by the first ensure of v2.
func TestEnsureDeletesTheSharedTunnelProbe(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	if err := database.GetDB().Create(&model.TunnelClient{ServerId: 1, UUID: "bbbbbbbb-0000-0000-0000-000000000002", Name: "probe-awg",
		Email: "probe-awg", Enable: true, IPv4Address: "10.66.66.2/32"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}}); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(awgProbePeers(t)); got != "probe-awg-ams-1-direct,probe-awg-ams-1-proxy" {
		t.Errorf("peers after the first v2 ensure = %s, want ams-1's pair without probe-awg", got)
	}
}

// TestEnsureRefusesBadMonClientIds (#120): the id becomes part of a peer
// name, so one outside 1–32 characters of [A-Za-z0-9_-] is 400 before
// anything changes.
func TestEnsureRefusesBadMonClientIds(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	for _, id := range []string{"", strings.Repeat("a", 33), "ams.1", "ams:1", "ams 1", "амс"} {
		_, err := m.EnsureProbeSet([]MonClient{{Id: "ok-1"}, {Id: id}})
		var monErr *MonError
		if !errors.As(err, &monErr) || monErr.Status != 400 || monErr.Code != "invalid_body" || !strings.Contains(monErr.Message, "monClients[1].id") {
			t.Errorf("id %q: err = %v, want 400 invalid_body", id, err)
		}
	}
	if subId, _ := (&SettingService{}).GetMonProbeSubId(); subId != "" || len(awgProbePeers(t)) != 0 {
		t.Error("a refused ensure changed the probe set")
	}
	if _, err := m.EnsureProbeSet([]MonClient{{Id: strings.Repeat("a", 32)}, {Id: "A_b-9"}}); err != nil {
		t.Errorf("valid ids: %v", err)
	}
}

// TestEnsureSurvivesAFullTunnelPool (#120): when the AmneziaWG pool has no
// room, ensure still answers, lists the mon-clients left without a peer, and
// their AWG items are missing from the configs; the others keep theirs.
func TestEnsureSurvivesAFullTunnelPool(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	// A /29 holds .2–.6 for clients (.1 is the server): room for five peers.
	db := database.GetDB()
	if err := db.Model(&model.TunnelServer{}).Where("1 = 1").Updates(map[string]any{"ipv4_pool": "10.66.66.0/29", "ipv4_address": "10.66.66.1/24"}).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := []MonClient{{Id: "a-1"}, {Id: "b-1"}, {Id: "c-1"}}
	res, err := m.EnsureProbeSet(snapshot)
	if err != nil {
		t.Fatalf("ensure with a full pool: %v", err)
	}
	if len(res.Unallocated) != 1 || res.Unallocated[0] != (MonUnallocated{MonClientId: "c-1", Path: "proxy", Reason: "pool_exhausted"}) ||
		res.Present != 5 || len(res.Created) != 5 {
		t.Errorf("ensure with a full pool: %+v", res)
	}
	direct, err := m.ProbeConfigs("203.0.113.10", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range direct.Items {
		got = append(got, it.MonClientId)
	}
	if strings.Join(got, ",") != "a-1,b-1,c-1" {
		t.Errorf("direct items for %v, want a-1, b-1 and c-1 (its direct peer fit)", got)
	}
	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	proxy, err := m.ProbeConfigs("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, it := range proxy.Items {
		got = append(got, it.MonClientId)
	}
	if strings.Join(got, ",") != "a-1,b-1" {
		t.Errorf("proxy items for %v, want a-1 and b-1 only", got)
	}

	// b-1 leaves: its addresses are freed first, so c-1 fits now.
	res, err = m.EnsureProbeSet([]MonClient{{Id: "a-1"}, {Id: "c-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unallocated) != 0 || res.Present != 4 {
		t.Errorf("ensure after room was freed: %+v", res)
	}
}

// TestEnsureCoversTheTunnelServer: ProbeConfigs renders one AmneziaWG item
// per mon-client for the asked path, named by monClientId, each the .conf of
// that mon-client's peer for the path with the chosen endpoint host; xray
// items carry no monClientId. DeleteProbeSet removes every peer.
func TestEnsureCoversTheTunnelServer(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "msk-1"}, {Id: "ams-1"}}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	peers := awgProbePeers(t)

	direct, err := m.ProbeConfigs("203.0.113.10", "", "")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if len(direct.Items) != 3 {
		t.Fatalf("direct items: %+v", direct.Items)
	}
	if it := direct.Items[0]; it.Kind != "awg" || it.MonClientId != "ams-1" || it.Filename != "probe-awg-ams-1-direct" ||
		!strings.Contains(it.Conf, "Endpoint = 203.0.113.10:") || !strings.Contains(it.Conf, peers["probe-awg-ams-1-direct"].PrivateKey) {
		t.Errorf("awg direct item 0: %+v", it)
	}
	if it := direct.Items[1]; it.MonClientId != "msk-1" || it.Filename != "probe-awg-msk-1-direct" {
		t.Errorf("awg direct item 1: %+v", it)
	}
	raw, _ := json.Marshal(direct.Items[2])
	if direct.Items[2].Kind != "xray" || strings.Contains(string(raw), "monClientId") {
		t.Errorf("xray item: %s", raw)
	}

	setSetting(t, "proxyOverrideEnable", "true")
	setSetting(t, "proxyOverrideHost", "front.example.net")
	proxy, err := m.ProbeConfigs("", "", "")
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if it := proxy.Items[0]; it.MonClientId != "ams-1" || it.Filename != "probe-awg-ams-1-proxy" ||
		!strings.Contains(it.Conf, "Endpoint = front.example.net:") || !strings.Contains(it.Conf, peers["probe-awg-ams-1-proxy"].PrivateKey) {
		t.Errorf("awg proxy item 0: %+v", it)
	}

	if err := m.DeleteProbeSet(); err != nil {
		t.Fatal(err)
	}
	if left := awgProbePeers(t); len(left) != 0 {
		t.Errorf("awg peers after delete: %s", sortedKeys(left))
	}
}

// TestRevisionCoversTheProbePeerSet (#120): a mon-client joining or leaving
// changes the peer set and so the revision, which is how mon-server learns
// to reread /probe/configs for the new mon-client.
func TestRevisionCoversTheProbePeerSet(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	one, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}, {Id: "msk-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if two.Revision == one.Revision {
		t.Error("a new mon-client's peers left the revision unchanged")
	}
	back, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if back.Revision == two.Revision {
		t.Error("a mon-client leaving left the revision unchanged")
	}
	if same, _ := m.EnsureProbeSet([]MonClient{{Id: "ams-1", State: "OFFLINE"}}); same.Revision != back.Revision {
		t.Error("a state change of a mon-client moved the revision")
	}
}

// multiLinks renders a multi-link inbound the way /sub does with several
// external proxies: one link per line.
type multiLinks struct{}

func (multiLinks) ProbeLink(inbound *model.Inbound, email, address, via string) string {
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

// TestProbeConfigsThroughAHop (contract 3 §4.4): ?hop= and its synonym
// ?edge= render the probe set with the hop's host, edge or inner, active or
// not, under the hop's path; an unknown name or two different names is 409
// unknown_hop, a pending or draining hop 409 hop_not_joined, and none of them
// ever falls back to the proxy path.
func TestProbeConfigsThroughAHop(t *testing.T) {
	m := newMonitoringTestService(t)
	awgTestServer(t)
	monInbound(t, 1, model.VLESS, true, model.Client{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice"})
	monHop(t, "core-1", "inner", "joined", 0, false, "10.0.0.7")
	monHop(t, "edge-a", "edge", "legacy", 0, true, "a.example.net")
	monHop(t, "edge-b", "edge", "pending", 0, false, "b.example.net")
	monHop(t, "edge-c", "edge", "draining", 0, false, "c.example.net")
	if _, err := m.EnsureProbeSet([]MonClient{{Id: "ams-1"}}); err != nil {
		t.Fatal(err)
	}
	peers := awgProbePeers(t)

	for _, tc := range []struct{ hop, edge, path, host string }{
		{"core-1", "", "inner:core-1", "10.0.0.7"},
		{"", "edge-a", "edge:edge-a", "a.example.net"},
		{"edge-a", "edge-a", "edge:edge-a", "a.example.net"},
		{" core-1 ", "", "inner:core-1", "10.0.0.7"},
	} {
		res, err := m.ProbeConfigs("203.0.113.10", tc.hop, tc.edge)
		if err != nil {
			t.Fatalf("hop=%q edge=%q: %v", tc.hop, tc.edge, err)
		}
		if res.Path != tc.path || len(res.Items) != 2 {
			t.Fatalf("hop=%q edge=%q: %+v", tc.hop, tc.edge, res)
		}
		awg, xr := res.Items[0], res.Items[1]
		peer := peers[ProbeTunnelEmail("ams-1", tc.path)]
		if awg.MonClientId != "ams-1" || awg.Filename != peer.Email || !strings.Contains(awg.Conf, "Endpoint = "+tc.host+":") ||
			!strings.Contains(awg.Conf, peer.PrivateKey) {
			t.Errorf("%s awg item: %+v", tc.path, awg)
		}
		if xr.Link != "vless://probe-1@"+tc.host+"/via" {
			t.Errorf("%s xray link = %q", tc.path, xr.Link)
		}
	}

	for _, tc := range []struct{ hop, edge, code string }{
		{"nope", "", "unknown_hop"},
		{"", "nope", "unknown_hop"},
		{"edge-a", "core-1", "unknown_hop"},
		{"edge-b", "", "hop_not_joined"},
		{"", "edge-c", "hop_not_joined"},
	} {
		_, err := m.ProbeConfigs("", tc.hop, tc.edge)
		var monErr *MonError
		if !errors.As(err, &monErr) || monErr.Status != 409 || monErr.Code != tc.code {
			t.Errorf("hop=%q edge=%q: err = %v, want 409 %s", tc.hop, tc.edge, err, tc.code)
		}
	}
	// Blank parameters are absent: the proxy path as before.
	if res, err := m.ProbeConfigs("", " ", ""); err != nil || res.Path != "proxy" {
		t.Errorf("blank hop: %+v, %v", res, err)
	}
}
