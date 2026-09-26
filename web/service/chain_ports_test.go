package service

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// newChainPortsService opens a panel on an empty temp database. The ports come
// from the panel's own tables — there is no generated xray config to write and
// no xray to restart before the list is right (#96).
func newChainPortsService(t *testing.T) *ChainPortsService {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	return &ChainPortsService{}
}

func portByNumber(ports []chain.Port, number int) (chain.Port, bool) {
	for _, port := range ports {
		if port.Port == number {
			return port, true
		}
	}
	return chain.Port{}, false
}

// addInbounds stores inbounds the way the panel's own writes leave them: with
// settings that are at least valid JSON, since the panel's own queries reach
// into them with SQLite's JSON functions.
func addInbounds(t *testing.T, inbounds ...model.Inbound) {
	t.Helper()
	db := database.GetDB()
	for index := range inbounds {
		if inbounds[index].Settings == "" {
			inbounds[index].Settings = `{"clients":[]}`
		}
		if err := db.Create(&inbounds[index]).Error; err != nil {
			t.Fatalf("create inbound %q: %v", inbounds[index].Tag, err)
		}
	}
}

// twoInbounds is the pair of ordinary xray inbounds most of these tests start
// from: one on every address, one on the default.
func twoInbounds(t *testing.T) {
	t.Helper()
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Port: 8443, Protocol: model.Trojan, Tag: "inbound-trojan", Remark: "trojan"},
		model.Inbound{UserId: 1, Enable: true, Listen: "0.0.0.0", Port: 443, Protocol: model.VLESS, Tag: "inbound-443", Remark: "vless"},
	)
}

// TestPortsFromEverySource is the list of §3.8 end to end: xray inbounds, an
// AmneziaWG server, an MTProto inbound and an operator extra, sorted by port
// so the document does not reshuffle itself between builds.
func TestPortsFromEverySource(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)
	db := database.GetDB()
	if err := db.Create(&model.TunnelServer{
		Kind: model.TunnelKindAwg, Enable: true, ListenPort: 51820,
	}).Error; err != nil {
		t.Fatalf("create tunnel server: %v", err)
	}
	if err := db.Create(&model.Inbound{
		UserId: 1, Enable: true, Port: 9443, Protocol: model.MTProto, Tag: "inbound-9443", Remark: "mtproto",
	}).Error; err != nil {
		t.Fatalf("create mtproto inbound: %v", err)
	}
	var mtproto model.Inbound
	if err := db.Where("protocol = ?", model.MTProto).First(&mtproto).Error; err != nil {
		t.Fatalf("reload mtproto inbound: %v", err)
	}
	if err := s.settingService.SetChainExtraPorts([]ChainExtraPort{{Port: 8080, Network: chain.NetworkTCP, Note: "stub site"}}); err != nil {
		t.Fatalf("SetChainExtraPorts: %v", err)
	}

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}

	want := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8080, Network: chain.NetworkTCP, Tag: "extra-8080", Source: chain.SourceExtra},
		{Port: 8443, Network: chain.NetworkTCPUDP, Tag: "inbound-trojan", Source: chain.SourceXray},
		{Port: 9443, Network: chain.NetworkTCP, Tag: "mtproto-" + strconv.Itoa(mtproto.Id), Source: chain.SourceMtproto},
		{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg},
	}
	if len(ports) != len(want) {
		t.Fatalf("Ports() = %+v, want %+v", ports, want)
	}
	for index, port := range ports {
		if port != want[index] {
			t.Errorf("port %d = %+v, want %+v", index, port, want[index])
		}
	}
}

// TestAnAddedInboundIsRelayedWithoutAnXrayRestart is the stand regression of
// #96: the ports used to be read from the generated bin/config.json, which the
// panel rewrites only when it restarts xray — long after the hook that bumps
// the revision has run. A front fetching the document in between cached the old
// list under the new revision and nothing ever bumped again. The table is the
// source now, so the port of an inbound the operator has just added is in the
// list the very next time it is composed.
func TestAnAddedInboundIsRelayedWithoutAnXrayRestart(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)

	before, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if _, found := portByNumber(before, 34567); found {
		t.Fatal("the port is relayed before its inbound exists")
	}

	// Added switched off and then switched on, which is the one path an
	// inbound can take through the panel's own writes with no xray running:
	// AddInbound talks to the live xray API for an enabled inbound, and
	// SetInboundEnable is the write the operator makes next.
	inbounds := &InboundService{}
	added := &model.Inbound{
		UserId: 1, Enable: false, Port: 34567, Protocol: model.VLESS,
		Tag: "inbound-34567", Remark: "just added", Settings: `{"clients":[],"decryption":"none"}`,
	}
	if _, _, err := inbounds.AddInbound(added); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	if _, err := inbounds.SetInboundEnable(added.Id, true); err != nil {
		t.Fatalf("SetInboundEnable: %v", err)
	}

	after, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	port, found := portByNumber(after, 34567)
	if !found {
		t.Fatalf("Ports() = %+v, want the port of the inbound just added", after)
	}
	if port.Source != chain.SourceXray || port.Tag != "inbound-34567" {
		t.Errorf("port = %+v, want the xray inbound's own tag", port)
	}
	if port.Network != chain.NetworkTCPUDP {
		t.Errorf("network = %q, want %q", port.Network, chain.NetworkTCPUDP)
	}
}

// TestDisabledSourcesAreNotRelayed: a switched-off inbound, tunnel or MTProto
// inbound has no listener — xray leaves a disabled inbound out of the config it
// generates — so a front relaying it would open a dead port.
func TestDisabledSourcesAreNotRelayed(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)
	addInbounds(t, model.Inbound{
		UserId: 1, Enable: false, Port: 2097, Protocol: model.VLESS, Tag: "switched-off", Remark: "off",
	})
	db := database.GetDB()
	if err := db.Create(&model.TunnelServer{Kind: model.TunnelKindWg, Enable: false, ListenPort: 51821}).Error; err != nil {
		t.Fatalf("create tunnel server: %v", err)
	}
	if err := db.Create(&model.Inbound{
		UserId: 1, Enable: false, Port: 9443, Protocol: model.MTProto, Tag: "off", Remark: "off",
	}).Error; err != nil {
		t.Fatalf("create mtproto inbound: %v", err)
	}

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if _, found := portByNumber(ports, 2097); found {
		t.Error("a disabled xray inbound is relayed")
	}
	if _, found := portByNumber(ports, 51821); found {
		t.Error("a disabled tunnel server is relayed")
	}
	if _, found := portByNumber(ports, 9443); found {
		t.Error("a disabled mtproto inbound is relayed")
	}
}

// TestTransparentInboundsAreNotRelayed: an inbound that only ever hears from
// the kernel (TPROXY / REDIRECT) cannot be dialed from outside, whichever of
// the three marks says so.
func TestTransparentInboundsAreNotRelayed(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Port: 12345, Protocol: "dokodemo-door", Tag: "awg-in",
			Remark: "tproxy", StreamSettings: `{"sockopt":{"tproxy":"tproxy"}}`},
		model.Inbound{UserId: 1, Enable: true, Port: 12346, Protocol: "dokodemo-door", Tag: "dnat",
			Remark: "redirect", Settings: `{"followRedirect":true}`},
		model.Inbound{UserId: 1, Enable: true, Port: 12347, Protocol: model.VLESS, Tag: "wg-tproxy-in",
			Remark: "by tag"},
	)

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	for _, port := range []int{12345, 12346, 12347} {
		if _, found := portByNumber(ports, port); found {
			t.Errorf("the transparent-proxy inbound on %d is relayed", port)
		}
	}
}

// TestTunnelProtocolsAreNotRelayedAsXrayPorts: AmneziaWG and WireGuard Native
// never enter the xray config — GetXrayConfig skips them — and their listeners
// come from the tunnel table instead, as UDP.
func TestTunnelProtocolsAreNotRelayedAsXrayPorts(t *testing.T) {
	s := newChainPortsService(t)
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Port: 51820, Protocol: model.AmneziaWG, Tag: "awg", Remark: "awg"},
		model.Inbound{UserId: 1, Enable: true, Port: 51822, Protocol: model.NativeWG, Tag: "wg", Remark: "wg"},
	)

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if len(ports) != 0 {
		t.Fatalf("Ports() = %+v, want nothing: neither protocol is served by xray", ports)
	}
}

// TestPublicPortReplacesTheInboundsOwnPort: behind the nginx front end an
// inbound lives on the loopback and clients reach it on 443. The fronts must
// relay what clients use, and the several inbounds multiplexed there are one
// relayed port, not several collisions.
func TestPublicPortReplacesTheInboundsOwnPort(t *testing.T) {
	s := newChainPortsService(t)
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 10443, Protocol: model.VLESS, Tag: "inbound-moved", Remark: "moved", PublicPort: PublicPort},
		model.Inbound{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 10444, Protocol: model.Trojan, Tag: "inbound-moved-2", Remark: "moved 2", PublicPort: PublicPort},
		model.Inbound{UserId: 1, Enable: true, Port: 2053, Protocol: model.VLESS, Tag: "inbound-2053", Remark: "plain"},
	)

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if _, found := portByNumber(ports, 10443); found {
		t.Error("the private port behind nginx is relayed; clients never use it")
	}
	public, found := portByNumber(ports, PublicPort)
	if !found {
		t.Fatalf("Ports() = %+v, want the public port %d", ports, PublicPort)
	}
	if public.Source != chain.SourceXray {
		t.Errorf("public port source = %q, want %q", public.Source, chain.SourceXray)
	}
	if _, found := portByNumber(ports, 2053); !found {
		t.Error("an inbound nginx did not move lost its port")
	}
	seen := 0
	for _, port := range ports {
		if port.Port == PublicPort {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the public port appears %d times, want once", seen)
	}
}

// TestChainPorts_MTProtoBehindTheFrontIsNotADuplicate is the bug of #140: an
// MTProto inbound moved behind nginx carries PublicPort 443, and so does the
// Reality inbound beside it. Both are multiplexed on the one front port, yet
// the MTProto source used to claim 443 a second time and the whole document
// was refused with duplicate_port — the chain stopped hearing any revision the
// moment the operator turned the front on.
func TestChainPorts_MTProtoBehindTheFrontIsNotADuplicate(t *testing.T) {
	s := newChainPortsService(t)
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 8443, Protocol: model.VLESS, Tag: "inbound-reality", Remark: "reality", PublicPort: PublicPort},
		model.Inbound{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 4343, Protocol: model.MTProto, Tag: "inbound-mtproto", Remark: "mtproto", PublicPort: PublicPort},
	)

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	want := []chain.Port{{Port: PublicPort, Network: chain.NetworkTCPUDP, Tag: "inbound-reality", Source: chain.SourceXray}}
	if len(ports) != 1 || ports[0] != want[0] {
		t.Errorf("Ports() = %+v, want %+v: the front port once, as the xray inbound's", ports, want)
	}
}

// TestChainPorts_MTProtoAloneBehindTheFrontIsRelayed: with nothing but the
// MTProto inbound behind nginx, 443 is still the port clients use, so it is
// still relayed — only as TCP, which is all MTProto speaks.
func TestChainPorts_MTProtoAloneBehindTheFrontIsRelayed(t *testing.T) {
	s := newChainPortsService(t)
	addInbounds(t,
		model.Inbound{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 4343, Protocol: model.MTProto, Tag: "inbound-mtproto", Remark: "mtproto", PublicPort: PublicPort},
	)

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if len(ports) != 1 || ports[0].Port != PublicPort || ports[0].Network != chain.NetworkTCP {
		t.Errorf("Ports() = %+v, want 443/tcp alone", ports)
	}
}

// TestLoopbackInboundsAreNotRelayed: an inbound bound to the loopback with no
// public port of its own is reachable by nothing outside the box.
func TestLoopbackInboundsAreNotRelayed(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)
	addInbounds(t, model.Inbound{
		UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 10445,
		Protocol: model.VLESS, Tag: "private", Remark: "private",
	})

	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if _, found := portByNumber(ports, 10445); found {
		t.Error("a loopback-only inbound is relayed")
	}
}

// TestDuplicatePortAcrossSourcesIsRefused: one port, one source (§3.8). The
// refusal names both, because the operator has to know which of the two to
// move.
func TestDuplicatePortAcrossSourcesIsRefused(t *testing.T) {
	s := newChainPortsService(t)
	twoInbounds(t)
	if err := s.settingService.SetChainExtraPorts([]ChainExtraPort{{Port: 443, Network: chain.NetworkTCP}}); err != nil {
		t.Fatalf("SetChainExtraPorts: %v", err)
	}

	_, err := s.Ports()
	if err == nil {
		t.Fatal("a port claimed by two sources was accepted")
	}
	if code := ChainErrorCode(err); code != CodeDuplicatePort {
		t.Fatalf("error code = %q, want %q", code, CodeDuplicatePort)
	}
	for _, want := range []string{chain.SourceXray, chain.SourceExtra} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the source %q", err, want)
		}
	}
}

// TestPortsOnAPanelWithNoInbounds: an empty panel publishes an empty list
// rather than failing — there is nothing to relay yet, and the chain is not
// broken by that.
func TestPortsOnAPanelWithNoInbounds(t *testing.T) {
	s := newChainPortsService(t)
	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports: %v", err)
	}
	if len(ports) != 0 {
		t.Fatalf("Ports() = %+v, want an empty list", ports)
	}
}
