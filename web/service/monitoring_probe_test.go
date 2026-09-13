package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

func TestIsProbeAccount(t *testing.T) {
	cases := map[string]bool{
		"probe-12":   true,
		"probe-awg":  true,
		"Probe-12":   true,
		"PROBE-AWG":  true,
		" probe-1":   true,
		"probe":      false,
		"probe12":    false,
		"alice":      false,
		"":           false,
		"myprobe-12": false,
	}
	for email, want := range cases {
		if got := IsProbeAccount(email); got != want {
			t.Errorf("IsProbeAccount(%q) = %v, want %v", email, got, want)
		}
	}
	if got := ProbeEmail(model.MonKindXray, 12); got != "probe-12" {
		t.Errorf("ProbeEmail(xray, 12) = %q", got)
	}
	if got := ProbeEmail(model.MonKindAwg, 0); got != "probe-awg" {
		t.Errorf("ProbeEmail(awg, 0) = %q", got)
	}
}

// clientsJSON renders an xray settings document with the given clients.
func clientsJSON(clients ...model.Client) string {
	raw, _ := json.Marshal(map[string]any{"clients": clients})
	return string(raw)
}

// seedXrayInbound stores a vless inbound with the given clients and their
// traffic rows, the way the panel keeps them.
func seedXrayInbound(t *testing.T, s *InboundService, tag string, enable bool, clients ...model.Client) *model.Inbound {
	t.Helper()
	db := database.GetDB()
	ib := &model.Inbound{
		UserId: 1, Remark: tag, Enable: enable, Port: 10000 + len(tag), Protocol: model.VLESS, Tag: tag,
		Settings:       clientsJSON(clients...),
		StreamSettings: `{"network":"tcp","security":"none"}`,
	}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}
	for i := range clients {
		if err := s.AddClientStat(db, ib.Id, &clients[i]); err != nil {
			t.Fatal(err)
		}
	}
	return ib
}

// TestProbePrefixRefusedOnEveryCreateAndEditPath: the ordinary client paths
// of xray inbounds and tunnels refuse a probe-prefixed email, and refuse to
// edit an existing probe account.
func TestProbePrefixRefusedOnEveryCreateAndEditPath(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := &InboundService{}
	// Disabled so that deleting it below does not go through the xray API,
	// which is not running here.
	probe := model.Client{ID: "11111111-1111-1111-1111-111111111111", Email: "probe-1", Enable: false}
	alice := model.Client{ID: "22222222-2222-2222-2222-222222222222", Email: "alice", Enable: true}
	ib := seedXrayInbound(t, s, "inbound-guard", true, probe, alice)

	t.Run("AddInboundClient", func(t *testing.T) {
		newProbe := model.Client{ID: "33333333-3333-3333-3333-333333333333", Email: "Probe-9", Enable: true}
		_, err := s.AddInboundClient(&model.Inbound{Id: ib.Id, Settings: clientsJSON(newProbe)})
		if !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("AddInboundClient accepted a probe email: %v", err)
		}
	})
	t.Run("UpdateInboundClient renaming a user into a probe", func(t *testing.T) {
		renamed := alice
		renamed.Email = "probe-alice"
		_, err := s.UpdateInboundClient(&model.Inbound{Id: ib.Id, Settings: clientsJSON(renamed)}, alice.ID)
		if !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("UpdateInboundClient accepted a probe email: %v", err)
		}
	})
	t.Run("UpdateInboundClient editing a probe", func(t *testing.T) {
		edited := probe
		edited.Email = "user-now"
		_, err := s.UpdateInboundClient(&model.Inbound{Id: ib.Id, Settings: clientsJSON(edited)}, probe.ID)
		if !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("UpdateInboundClient edited a probe account: %v", err)
		}
	})

	awg := &AwgService{}
	t.Run("TunnelService.AddClient", func(t *testing.T) {
		err := awg.AddClient(&model.TunnelClient{Name: "p", Email: "probe-awg", Enable: true})
		if !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("AddClient accepted a probe email: %v", err)
		}
	})
	t.Run("TunnelService.UpdateClient", func(t *testing.T) {
		server, err := awg.GetServer()
		if err != nil {
			t.Fatal(err)
		}
		stored := &model.TunnelClient{ServerId: server.Id, UUID: "aaaaaaaa-0000-0000-0000-00000000000a",
			Name: "probe-awg", Email: "probe-awg", Enable: true, IPv4Address: "10.66.66.9/32"}
		if err := database.GetDB().Create(stored).Error; err != nil {
			t.Fatal(err)
		}
		edited := *stored
		edited.Email = "bob"
		if err := awg.UpdateClient(&edited); !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("UpdateClient edited a probe peer: %v", err)
		}
		user := &model.TunnelClient{ServerId: server.Id, UUID: "bbbbbbbb-0000-0000-0000-00000000000b",
			Name: "bob", Email: "bob", Enable: true, IPv4Address: "10.66.66.10/32"}
		if err := database.GetDB().Create(user).Error; err != nil {
			t.Fatal(err)
		}
		user.Email = "probe-bob"
		if err := awg.UpdateClient(user); !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("UpdateClient renamed a user into a probe: %v", err)
		}
		if err := awg.UpdateClientByUUID(stored.UUID, &edited); !errors.Is(err, errProbeAccountReserved) {
			t.Fatalf("UpdateClientByUUID edited a probe peer: %v", err)
		}
	})

	t.Run("deleting a probe stays allowed", func(t *testing.T) {
		if _, err := s.DelInboundClient(ib.Id, probe.ID); err != nil {
			t.Fatalf("DelInboundClient refused a probe: %v", err)
		}
		after, err := s.GetInbound(ib.Id)
		if err != nil {
			t.Fatal(err)
		}
		clients, _ := s.GetClients(after)
		if len(clients) != 1 || clients[0].Email != "alice" {
			t.Fatalf("clients after deleting the probe: %+v", clients)
		}
	})
}

// TestOnlineSetsExcludeProbeAccounts: the online lists the UI, the traffic
// broadcast and the bot count from never contain a probe, for xray and for the
// tunnel flavours alike.
func TestOnlineSetsExcludeProbeAccounts(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	now := time.Now().UnixMilli()
	ib := seedXrayInbound(t, &InboundService{}, "inbound-online", true)
	for _, email := range []string{"probe-1", "alice", "Probe-2"} {
		if err := db.Create(&xray.ClientTraffic{InboundId: ib.Id, Email: email, Enable: true, LastOnline: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	awg := &AwgService{}
	server, err := awg.GetServer()
	if err != nil {
		t.Fatal(err)
	}
	for i, email := range []string{"probe-awg", "bob"} {
		c := &model.TunnelClient{ServerId: server.Id, UUID: fmt.Sprintf("cccccccc-0000-0000-0000-00000000000%d", i),
			Name: email, Email: email, Enable: true, IPv4Address: fmt.Sprintf("10.66.66.%d/32", 20+i), LastOnline: now}
		if err := db.Create(c).Error; err != nil {
			t.Fatal(err)
		}
	}

	online := (&InboundService{}).GetOnlineClients()
	if len(online) != 2 {
		t.Fatalf("GetOnlineClients = %v, want alice and bob's uuid only", online)
	}
	for _, id := range online {
		if IsProbeAccount(id) || id == "cccccccc-0000-0000-0000-000000000000" {
			t.Fatalf("GetOnlineClients leaks a probe: %v", online)
		}
	}
	tunnelOnline := awg.GetOnlineClients()
	if len(tunnelOnline) != 1 || tunnelOnline[0] != "cccccccc-0000-0000-0000-000000000001" {
		t.Fatalf("tunnel GetOnlineClients = %v, want bob's uuid only", tunnelOnline)
	}
	if got := withoutProbeAccounts([]string{"probe-1", "alice", "PROBE-x"}); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("withoutProbeAccounts = %v", got)
	}
}

// TestProbeXrayClientAttributes: the probe account carries §3.3 — enabled, no
// limits, the set's subId, the first user's flow, and a credential that fits
// the protocol.
func TestProbeXrayClientAttributes(t *testing.T) {
	vless := &model.Inbound{Id: 12, Protocol: model.VLESS}
	c := probeXrayClient(vless, []model.Client{{Email: "probe-12", Flow: "old"}, {Email: "alice", Flow: "xtls-rprx-vision"}}, "sub16")
	if c.Email != "probe-12" || !c.Enable || c.TotalGB != 0 || c.ExpiryTime != 0 || c.LimitIP != 0 ||
		c.TgID != 0 || c.Reset != 0 || c.SubID != "sub16" || c.Comment != probeComment {
		t.Fatalf("attributes: %+v", c)
	}
	if c.Flow != "xtls-rprx-vision" {
		t.Errorf("flow %q, want the first regular client's", c.Flow)
	}
	if len(c.ID) != 36 {
		t.Errorf("vless probe has no uuid: %q", c.ID)
	}
	if c2 := probeXrayClient(vless, nil, "sub16"); c2.Flow != "" {
		t.Errorf("flow with no regular clients should be empty, got %q", c2.Flow)
	}

	trojan := probeXrayClient(&model.Inbound{Id: 3, Protocol: model.Trojan}, nil, "s")
	if trojan.ID != "" || trojan.Password == "" {
		t.Errorf("trojan probe: %+v", trojan)
	}
	ss := probeXrayClient(&model.Inbound{Id: 4, Protocol: model.Shadowsocks, Settings: `{"method":"2022-blake3-aes-128-gcm"}`}, nil, "s")
	if len(ss.Password) != 24 { // base64 of 16 bytes
		t.Errorf("ss2022 aes-128 probe password %q, want a 16-byte base64 key", ss.Password)
	}
	ss256 := probeXrayClient(&model.Inbound{Id: 5, Protocol: model.Shadowsocks, Settings: `{"method":"2022-blake3-aes-256-gcm"}`}, nil, "s")
	if len(ss256.Password) != 44 { // base64 of 32 bytes
		t.Errorf("ss2022 aes-256 probe password %q, want a 32-byte base64 key", ss256.Password)
	}
	classic := probeXrayClient(&model.Inbound{Id: 6, Protocol: model.Shadowsocks, Settings: `{"method":"aes-256-gcm"}`}, nil, "s")
	if classic.Password == "" {
		t.Errorf("classic ss probe has no password")
	}

	peer := probeTunnelClient()
	if peer.Email != "probe-awg" || !peer.Enable || peer.Comment != probeComment {
		t.Errorf("tunnel probe: %+v", peer)
	}
}

// TestTunnelProbeGrantIsSingleUse: a grant lets exactly one AddClient through
// and only for the client value it was issued for.
func TestTunnelProbeGrantIsSingleUse(t *testing.T) {
	a := probeTunnelClient()
	b := probeTunnelClient()
	grantTunnelProbe(a)
	if takeTunnelProbeGrant(b) {
		t.Fatal("a grant for a let b through")
	}
	if !takeTunnelProbeGrant(a) {
		t.Fatal("grant for a was not honoured")
	}
	if takeTunnelProbeGrant(a) {
		t.Fatal("grant for a was reusable")
	}
}
