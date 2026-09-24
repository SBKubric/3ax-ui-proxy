package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/tunnel"
)

// TestTunnelFlavoursStayIsolated is the regression test for the schema merge:
// AmneziaWG and native WireGuard now share tunnel_servers/tunnel_clients, and
// every query in either service must stay inside its own flavour. Before the
// merge that isolation came free from having separate tables — now it is a
// property of the code, so it needs a test.
func TestTunnelFlavoursStayIsolated(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	awg, wg := &AwgService{}, &WgService{}

	awgServer, err := awg.GetServer()
	if err != nil {
		t.Fatalf("AWG GetServer: %v", err)
	}
	wgServer, err := wg.GetServer()
	if err != nil {
		t.Fatalf("WG GetServer: %v", err)
	}

	// Two distinct rows, correctly tagged.
	if awgServer.Id == wgServer.Id {
		t.Fatalf("both services got the same server row (id %d)", awgServer.Id)
	}
	if awgServer.Kind != model.TunnelKindAwg || wgServer.Kind != model.TunnelKindWg {
		t.Fatalf("kinds are wrong: awg=%q wg=%q", awgServer.Kind, wgServer.Kind)
	}
	// GetServer is called on every request; it must not keep creating rows.
	if _, err := awg.GetServer(); err != nil {
		t.Fatal(err)
	}
	var servers int64
	db.Model(&model.TunnelServer{}).Count(&servers)
	if servers != 2 {
		t.Fatalf("tunnel_servers has %d rows, want 2", servers)
	}

	// Clients of one flavour must be invisible to the other, including the
	// lookups by id and uuid, which used to be scoped by the table itself.
	awgClient := model.TunnelClient{ServerId: awgServer.Id, UUID: "aaaaaaaa-0000-0000-0000-000000000001",
		Name: "alice", Email: "alice", Enable: true, IPv4Address: "10.66.66.2/32"}
	wgClient := model.TunnelClient{ServerId: wgServer.Id, UUID: "bbbbbbbb-0000-0000-0000-000000000002",
		Name: "bob", Email: "bob", Enable: true, IPv4Address: "10.77.77.2/32"}
	for _, c := range []*model.TunnelClient{&awgClient, &wgClient} {
		if err := db.Create(c).Error; err != nil {
			t.Fatal(err)
		}
	}

	awgClients, err := awg.GetClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(awgClients) != 1 || awgClients[0].UUID != awgClient.UUID {
		t.Fatalf("AWG GetClients returned %d rows, want only its own: %+v", len(awgClients), awgClients)
	}
	wgClients, err := wg.GetClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(wgClients) != 1 || wgClients[0].UUID != wgClient.UUID {
		t.Fatalf("WG GetClients returned %d rows, want only its own: %+v", len(wgClients), wgClients)
	}

	if _, err := awg.GetClient(wgClient.Id); err == nil {
		t.Error("AWG service fetched a WireGuard client by id")
	}
	if _, err := awg.GetClientByUUID(wgClient.UUID); err == nil {
		t.Error("AWG service fetched a WireGuard client by uuid")
	}
	if _, err := wg.GetClientByUUID(awgClient.UUID); err == nil {
		t.Error("WG service fetched an AmneziaWG client by uuid")
	}

	// Online lists are keyed by uuid and feed the inbounds page — they must not
	// bleed across flavours either.
	db.Model(&model.TunnelClient{}).Where("id = ?", wgClient.Id).
		Update("last_online", nowMilli())
	for _, uuid := range awg.GetOnlineClients() {
		if uuid == wgClient.UUID {
			t.Error("a WireGuard client showed up in the AmneziaWG online list")
		}
	}
}

func nowMilli() int64 {
	return 9_999_999_999_999
}

// TestRouteViaXrayReadsMergedTables guards the wiring that turns RouteViaXray
// into something that actually works: with the flag on, the panel must inject a
// dokodemo-door inbound for the tunnel and offer its tag for routing rules.
// Both used to be read from the legacy tables, which the services stopped
// writing after the schema merge — the tunnel would then have had its traffic
// redirected to a port nothing was listening on.
func TestRouteViaXrayReadsMergedTables(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()

	awg := &AwgService{}
	server, err := awg.GetServer()
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if err := db.Model(&model.TunnelServer{}).Where("id = ?", server.Id).
		Updates(map[string]any{"enable": true, "route_via_xray": true}).Error; err != nil {
		t.Fatal(err)
	}

	inbounds := tunnelTproxyInbounds()
	if len(inbounds) != 1 {
		t.Fatalf("expected one TPROXY inbound for the enabled tunnel, got %d", len(inbounds))
	}
	if inbounds[0].Tag != server.XrayInboundTag {
		t.Errorf("inbound tag = %q, want %q", inbounds[0].Tag, server.XrayInboundTag)
	}
	if inbounds[0].Port != server.XrayTproxyPort {
		t.Errorf("inbound port = %d, want %d", inbounds[0].Port, server.XrayTproxyPort)
	}

	tags, err := (&InboundService{}).GetInboundTags()
	if err != nil {
		t.Fatalf("GetInboundTags: %v", err)
	}
	if !strings.Contains(tags, server.XrayInboundTag) {
		t.Errorf("routing tag list %s does not offer %q", tags, server.XrayInboundTag)
	}

	// Turning it off withdraws both.
	if err := db.Model(&model.TunnelServer{}).Where("id = ?", server.Id).
		Update("route_via_xray", false).Error; err != nil {
		t.Fatal(err)
	}
	if got := tunnelTproxyInbounds(); len(got) != 0 {
		t.Errorf("TPROXY inbound survived turning RouteViaXray off: %+v", got)
	}
}

// TestFreshServersGetTheirOwnDefaults: with both flavours in one table the
// schema can no longer say "10.66.66.0/24, but 10.77.77.0/24 for the other
// one", so the service fills those in. A WireGuard server inheriting the
// AmneziaWG subnet would collide with a live tunnel on the same host.
func TestFreshServersGetTheirOwnDefaults(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	awgServer, err := (&AwgService{}).GetServer()
	if err != nil {
		t.Fatal(err)
	}
	wgServer, err := (&WgService{}).GetServer()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		got    *model.TunnelServer
		iface  string
		pool   string
		tag    string
		tproxy int
	}{
		{"awg", awgServer, "awg0", "10.66.66.0/24", "awg-tproxy-in", 12345},
		{"wg", wgServer, "wg0", "10.77.77.0/24", "wg-tproxy-in", 12346},
	} {
		if c.got.InterfaceName != c.iface {
			t.Errorf("%s: interface = %q, want %q", c.name, c.got.InterfaceName, c.iface)
		}
		if c.got.IPv4Pool != c.pool {
			t.Errorf("%s: pool = %q, want %q", c.name, c.got.IPv4Pool, c.pool)
		}
		if c.got.XrayInboundTag != c.tag {
			t.Errorf("%s: tproxy tag = %q, want %q", c.name, c.got.XrayInboundTag, c.tag)
		}
		if c.got.XrayTproxyPort != c.tproxy {
			t.Errorf("%s: tproxy port = %d, want %d", c.name, c.got.XrayTproxyPort, c.tproxy)
		}
	}
	if awgServer.ListenPort == wgServer.ListenPort {
		t.Errorf("both tunnels picked the same listen port %d", awgServer.ListenPort)
	}
}

// TestFreshAwgServerIsBornObfuscated: a fresh install used to serve the 1.x
// defaults — no junk packets at all — until someone found the Generate button,
// by which time changing the set disconnects the clients already on it. The
// record is now seeded at creation, and only at creation.
func TestFreshAwgServerIsBornObfuscated(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	server, err := (&AwgService{}).GetServer()
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if server.Jc == 0 || server.Jmin == 0 || server.Jmax == 0 {
		t.Errorf("junk packets left off: Jc=%d Jmin=%d Jmax=%d", server.Jc, server.Jmin, server.Jmax)
	}
	if server.S1 == 0 || server.S2 == 0 {
		t.Errorf("handshake padding left off: S1=%d S2=%d", server.S1, server.S2)
	}
	for i, h := range []string{server.H1, server.H2, server.H3, server.H4} {
		if !strings.Contains(h, "-") {
			t.Errorf("H%d = %q is not a 2.0 range", i+1, h)
		}
	}
	for i, iv := range []string{server.I1, server.I2, server.I3, server.I4, server.I5} {
		if iv == "" {
			t.Errorf("I%d was not generated", i+1)
		}
	}
	if err := tunnel.ValidateObfuscation(&model.TunnelServer{
		Jc: server.Jc, Jmin: server.Jmin, Jmax: server.Jmax,
		S1: server.S1, S2: server.S2, S3: server.S3, S4: server.S4,
		H1: server.H1, H2: server.H2, H3: server.H3, H4: server.H4,
		HeaderProtectionKey:    server.HeaderProtectionKey,
		ContentPaddingAddition: server.ContentPaddingAddition,
		RekeyAfterTime:         server.RekeyAfterTime,
		RejectAfterTime:        server.RejectAfterTime,
	}); err != nil {
		t.Errorf("the seeded set does not validate: %v", err)
	}

	// Second call must not regenerate: the operator's own values would be
	// overwritten on every page load.
	again, err := (&AwgService{}).GetServer()
	if err != nil {
		t.Fatalf("GetServer again: %v", err)
	}
	if again.Jc != server.Jc || again.H1 != server.H1 || again.I2 != server.I2 {
		t.Errorf("the set was regenerated on a later read")
	}

	// An existing record — the upgrade path — keeps whatever it had.
	db := database.GetDB()
	if err := db.Model(&model.TunnelServer{}).Where("id = ?", server.Id).
		Updates(map[string]any{"jc": 0, "jmin": 0, "jmax": 0, "s1": 0, "s2": 0,
			"h1": "1", "h2": "2", "h3": "3", "h4": "4", "i1": "", "i2": ""}).Error; err != nil {
		t.Fatal(err)
	}
	legacy, err := (&AwgService{}).GetServer()
	if err != nil {
		t.Fatalf("GetServer legacy: %v", err)
	}
	if legacy.Jc != 0 || legacy.H1 != "1" || legacy.I1 != "" {
		t.Errorf("a 1.x server was rewritten on read: Jc=%d H1=%q I1=%q", legacy.Jc, legacy.H1, legacy.I1)
	}

	// Native WireGuard has no obfuscation to seed.
	wg, err := (&WgService{}).GetServer()
	if err != nil {
		t.Fatalf("wg GetServer: %v", err)
	}
	if wg.Jc != 0 || wg.H1 != "" || wg.HeaderProtectionKey != "" {
		t.Errorf("obfuscation leaked into a WireGuard server: %+v", wg)
	}
}

// The proxy-front host override must reach tunnel client configs: with it on,
// the Endpoint names the proxy front (same port), and the stored server row is
// left untouched so switching the override off restores the real endpoint.
func TestWithProxyOverrideRewritesEndpointOnly(t *testing.T) {
	server := &tunnel.Server{Endpoint: "1.2.3.4:51820", ListenPort: 51820}

	if got := withProxyOverride(server, "proxy.example.com", false); got != server {
		t.Fatal("override off: expected the same server pointer back")
	}
	if got := withProxyOverride(server, "", true); got != server {
		t.Fatal("override on with empty host: expected the same server pointer back")
	}

	got := withProxyOverride(server, "proxy.example.com", true)
	if got.Endpoint != "proxy.example.com:51820" {
		t.Errorf("Endpoint = %q, want proxy.example.com:51820", got.Endpoint)
	}
	if server.Endpoint != "1.2.3.4:51820" {
		t.Errorf("stored server mutated: Endpoint = %q", server.Endpoint)
	}

	conf := tunnel.GenerateClientConfig(tunnel.AWG, got, tunnel.Client{IPv4Address: "10.0.0.2/32"})
	if !strings.Contains(conf, "Endpoint = proxy.example.com:51820\n") {
		t.Errorf("client conf does not carry the proxy endpoint:\n%s", conf)
	}
	if strings.Contains(conf, "1.2.3.4") {
		t.Errorf("client conf leaks the real server address:\n%s", conf)
	}
}

// SBKubric/3ax-ui-proxy#118: records from before the S4 padding was accounted
// for carry the legacy 1420, which with S4 = 22 puts a full-size packet at 1502
// bytes. Reading the server lowers such a panel-picked MTU to fit; an MTU the
// operator chose is left alone.
func TestLegacyDefaultMTUFollowsPadding(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	svc := &AwgService{}
	fresh, err := svc.GetServer()
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if fresh.S4 == 0 || fresh.MTU != tunnel.DefaultMTU(fresh.S4) {
		t.Errorf("fresh server: MTU %d with S4 %d, want %d", fresh.MTU, fresh.S4, tunnel.DefaultMTU(fresh.S4))
	}

	db := database.GetDB()
	setMTU := func(mtu, s4 int) {
		t.Helper()
		if err := db.Model(&model.TunnelServer{}).Where("id = ?", fresh.Id).
			Updates(map[string]any{"mtu": mtu, "s4": s4}).Error; err != nil {
			t.Fatal(err)
		}
	}

	setMTU(1420, 22) // the stand, upgraded
	got, err := svc.GetServer()
	if err != nil {
		t.Fatal(err)
	}
	if got.MTU != 1398 {
		t.Errorf("legacy 1420 with S4=22 read back as %d, want 1398", got.MTU)
	}
	var stored model.TunnelServer
	db.First(&stored, fresh.Id)
	if stored.MTU != 1398 {
		t.Errorf("the lowered MTU was not persisted: %d", stored.MTU)
	}
	conf, err := svc.GetClientConfig(mustAddClient(t, svc).Id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "MTU = 1398\n") {
		t.Errorf("client config does not carry the lowered MTU:\n%s", conf)
	}

	setMTU(1300, 22) // chosen by the operator
	if got, _ := svc.GetServer(); got.MTU != 1300 {
		t.Errorf("operator MTU 1300 was rewritten to %d", got.MTU)
	}

	setMTU(1420, 0) // a 1.x server: 1420 is right
	if got, _ := svc.GetServer(); got.MTU != 1420 {
		t.Errorf("unpadded server's 1420 was rewritten to %d", got.MTU)
	}

	wg, err := (&WgService{}).GetServer()
	if err != nil {
		t.Fatal(err)
	}
	if wg.MTU != 1420 {
		t.Errorf("WireGuard MTU = %d, want 1420", wg.MTU)
	}
}

// Saving follows the default MTU along with the padding (Generate changes S4
// while the form still holds the MTU computed for the old value) and refuses
// an MTU the padding cannot fit.
func TestSaveServerKeepsMTUInsideTheLink(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	svc := &AwgService{}
	server, err := svc.GetServer()
	if err != nil {
		t.Fatal(err)
	}
	server.Enable = false // nothing to apply to the host in a test

	// Pin the padding first; the MTU follows from the seeded default.
	server.S4 = 22
	if err := svc.SaveServer(server); err != nil {
		t.Fatalf("save S4=22: %v", err)
	}
	if server.MTU != 1398 {
		t.Fatalf("MTU after S4=22 = %d, want 1398", server.MTU)
	}

	// Generate: new S4, the form still says 1398.
	server.S4 = 10
	if err := svc.SaveServer(server); err != nil {
		t.Fatalf("save S4=10: %v", err)
	}
	if server.MTU != 1410 {
		t.Errorf("default MTU did not follow S4 22 -> 10: %d, want 1410", server.MTU)
	}

	// The legacy value in a POST (an old form, an orchestrator inventory) is a
	// default too.
	server.MTU = 1420
	if err := svc.SaveServer(server); err != nil {
		t.Fatalf("save MTU=1420: %v", err)
	}
	if server.MTU != 1410 {
		t.Errorf("posted 1420 saved as %d, want the default 1410", server.MTU)
	}

	// An explicit value over the IPv4 ceiling is refused and nothing is stored.
	server.MTU = 1431
	if err := svc.SaveServer(server); err == nil {
		t.Errorf("MTU 1431 with S4=10 was accepted; the ceiling is 1430")
	}
	var stored model.TunnelServer
	database.GetDB().First(&stored, server.Id)
	if stored.MTU != 1410 {
		t.Errorf("refused save still changed the MTU to %d", stored.MTU)
	}

	// An explicit value that fits is kept as typed.
	server.MTU = 1380
	if err := svc.SaveServer(server); err != nil {
		t.Fatalf("save MTU=1380: %v", err)
	}
	if stored = (model.TunnelServer{}); database.GetDB().First(&stored, server.Id).Error != nil || stored.MTU != 1380 {
		t.Errorf("operator MTU 1380 stored as %d", stored.MTU)
	}
}

func mustAddClient(t *testing.T, svc *AwgService) *model.TunnelClient {
	t.Helper()
	c := &model.TunnelClient{Name: "mtu-check", Email: "mtu-check", Enable: false}
	if err := svc.AddClient(c); err != nil {
		t.Fatalf("AddClient: %v", err)
	}
	return c
}
