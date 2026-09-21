package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// newChainPortsService opens a panel with an empty registry and a fake xray
// config: the ports service reads the config the panel wrote, so a test only
// has to write the file the running panel would have.
func newChainPortsService(t *testing.T, xrayConfig string) *ChainPortsService {
	t.Helper()
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(xrayConfig), 0o600); err != nil {
		t.Fatalf("write xray config: %v", err)
	}
	return &ChainPortsService{xrayConfigPath: path}
}

func portByNumber(ports []chain.Port, number int) (chain.Port, bool) {
	for _, port := range ports {
		if port.Port == number {
			return port, true
		}
	}
	return chain.Port{}, false
}

const twoInbounds = `{"inbounds":[
  {"listen":"127.0.0.1","port":62789,"protocol":"dokodemo-door","tag":"api"},
  {"port":8443,"protocol":"trojan","tag":"inbound-trojan"},
  {"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443"}]}`

// TestPortsFromEverySource is the list of §3.8 end to end: xray inbounds, an
// AmneziaWG server, an MTProto inbound and an operator extra, sorted by port
// so the document does not reshuffle itself between builds.
func TestPortsFromEverySource(t *testing.T) {
	s := newChainPortsService(t, twoInbounds)
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

// TestDisabledSourcesAreNotRelayed: a switched-off tunnel or MTProto inbound
// has no listener, so a front relaying it would open a dead port.
func TestDisabledSourcesAreNotRelayed(t *testing.T) {
	s := newChainPortsService(t, twoInbounds)
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
	if _, found := portByNumber(ports, 51821); found {
		t.Error("a disabled tunnel server is relayed")
	}
	if _, found := portByNumber(ports, 9443); found {
		t.Error("a disabled mtproto inbound is relayed")
	}
}

// TestPublicPortReplacesTheInboundsOwnPort: behind the nginx front end an
// inbound lives on the loopback and clients reach it on 443. The fronts must
// relay what clients use, and the several inbounds multiplexed there are one
// relayed port, not several collisions.
func TestPublicPortReplacesTheInboundsOwnPort(t *testing.T) {
	config := `{"inbounds":[
	  {"listen":"127.0.0.1","port":10443,"protocol":"vless","tag":"inbound-moved"},
	  {"listen":"127.0.0.1","port":10444,"protocol":"trojan","tag":"inbound-moved-2"},
	  {"port":2053,"protocol":"vless","tag":"inbound-2053"}]}`
	s := newChainPortsService(t, config)
	db := database.GetDB()
	for _, inbound := range []model.Inbound{
		{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 10443, Protocol: model.VLESS, Tag: "inbound-moved", Remark: "moved", PublicPort: PublicPort},
		{UserId: 1, Enable: true, Listen: "127.0.0.1", Port: 10444, Protocol: model.Trojan, Tag: "inbound-moved-2", Remark: "moved 2", PublicPort: PublicPort},
		{UserId: 1, Enable: true, Port: 2053, Protocol: model.VLESS, Tag: "inbound-2053", Remark: "plain"},
	} {
		if err := db.Create(&inbound).Error; err != nil {
			t.Fatalf("create inbound: %v", err)
		}
	}

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

// TestDuplicatePortAcrossSourcesIsRefused: one port, one source (§3.8). The
// refusal names both, because the operator has to know which of the two to
// move.
func TestDuplicatePortAcrossSourcesIsRefused(t *testing.T) {
	s := newChainPortsService(t, twoInbounds)
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

// TestPortsResolveTheXrayConfigFromAnyCwd is the regression for `x-ui chain
// ports` failing with "open bin/config.json: no such file or directory" when
// run from an arbitrary directory (e.g. an operator's `/root` shell): with no
// override, ChainPortsService reads config.GetConfigPath(), which used to be
// resolved against the process's cwd rather than the panel's install folder.
// It must find the config regardless of where the process happens to be
// running from.
func TestPortsResolveTheXrayConfigFromAnyCwd(t *testing.T) {
	if os.Getenv("XUI_BIN_FOLDER") != "" {
		t.Skip("XUI_BIN_FOLDER is set; this test exercises the unset default")
	}

	// GetBinFolderPath only resolves against the executable's own folder when
	// that folder's bin/ actually exists (its fallback for `go test`/`go run`
	// is the plain relative "bin"), so the exe-relative folder has to be
	// created before asking the function for it, not looked up first.
	exePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	binDir := filepath.Join(filepath.Dir(exePath), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll bin dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(binDir) })
	if got := config.GetBinFolderPath(); got != binDir {
		t.Fatalf("GetBinFolderPath() = %q, want the executable-relative folder %q now that it exists", got, binDir)
	}

	configPath := filepath.Join(binDir, "config.json")
	if err := os.WriteFile(configPath, []byte(twoInbounds), 0o600); err != nil {
		t.Fatalf("write xray config: %v", err)
	}

	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir to a foreign cwd: %v", err)
	}

	s := &ChainPortsService{}
	ports, err := s.Ports()
	if err != nil {
		t.Fatalf("Ports() from a cwd with no bin/config.json of its own: %v", err)
	}
	if _, found := portByNumber(ports, 443); !found {
		t.Errorf("Ports() = %+v, want the xray inbound on 443", ports)
	}
}

// TestPortsWithoutAnXrayConfig refuses rather than returns an empty list: an
// empty list is a valid document that tells every front to relay nothing, and
// a missing config file is not a reason to take the chain off the air.
func TestPortsWithoutAnXrayConfig(t *testing.T) {
	s := newChainPortsService(t, twoInbounds)
	s.xrayConfigPath = filepath.Join(t.TempDir(), "gone.json")
	if _, err := s.Ports(); err == nil {
		t.Fatal("a missing xray config produced a port list")
	}
}
