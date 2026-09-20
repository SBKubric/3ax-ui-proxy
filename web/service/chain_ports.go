package service

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/chainports"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/xray"

	"gorm.io/gorm"
)

// CodeDuplicatePort is the refusal of "one port, one source" (§3.8): two
// services claiming the same port cannot both be relayed, and picking a winner
// silently would take the other off the air with nothing said.
const CodeDuplicatePort = "duplicate_port"

// ChainPortsService computes the relayed-port list of the chain document
// (docs/spec/proxy-chain.md §3.8). Four sources, in the order the spec lists
// them: the xray inbounds the panel actually runs, the tunnel servers, the
// MTProto inbounds xray never sees, and the operator's own extras.
//
// It reads, it never writes: the ports are derived from the panel's current
// state every time a document is built, so there is no second copy of them to
// fall out of date. What moves the chain is the revision (§3.4), and the hooks
// that bump it live with the things they change.
type ChainPortsService struct {
	settingService SettingService

	// db binds every query to one handle. The port hooks run inside the
	// transaction of the write that changed the ports, and this SQLite has a
	// single connection: a query of our own there would wait for the
	// connection that transaction is holding. Nil means the global handle.
	db *gorm.DB

	// xrayConfigPath overrides the running panel's config path. Only tests
	// set it; the panel has exactly one config and it is the one xray runs.
	xrayConfigPath string
}

// lastChainPortsProblem is the last composition refusal, kept for the editor's
// banner (§3.8): the panel refuses to publish a port list it cannot make sense
// of, and the operator has to be told which two sources collide. It is
// package-level because whoever reads it — a UI request — is not the caller
// that composed the list.
var lastChainPortsProblem atomic.Pointer[ChainError]

// LastProblem returns the composition problem the last build ran into, or nil
// when the ports came out clean. It is a snapshot, not a subscription.
func (s *ChainPortsService) LastProblem() *ChainError {
	return lastChainPortsProblem.Load()
}

// recordPortsProblem remembers a refusal the banner can explain and forgets it
// as soon as a build succeeds. An error that is not a ChainError — an
// unreadable xray config, say — is a fault of the panel's own state rather
// than of the port composition, so it clears the banner instead of filling it
// with something the operator cannot act on in the chain editor.
func recordPortsProblem(err error) {
	var chainErr *ChainError
	if errors.As(err, &chainErr) {
		lastChainPortsProblem.Store(chainErr)
		return
	}
	lastChainPortsProblem.Store(nil)
}

// handle is the connection every query goes through.
func (s *ChainPortsService) handle() *gorm.DB {
	if s.db != nil {
		return s.db
	}
	return database.GetDB()
}

func (s *ChainPortsService) configPath() string {
	if s.xrayConfigPath != "" {
		return s.xrayConfigPath
	}
	return xray.GetConfigPath()
}

// Ports returns the relayed ports, sorted by port number so two builds of the
// same state are byte-identical — a document that reshuffled its ports would
// look like a change to every box that reads it.
func (s *ChainPortsService) Ports() ([]chain.Port, error) {
	ports, err := s.compose()
	recordPortsProblem(err)
	return ports, err
}

// compose is Ports without the bookkeeping.
func (s *ChainPortsService) compose() ([]chain.Port, error) {
	db := s.handle()
	extra := []ChainExtraPort{}
	if db != nil {
		raw, err := getSettingTx(db, chainExtraPortsKey)
		if err != nil {
			return nil, err
		}
		extra, err = parseChainExtraPorts(raw)
		if err != nil {
			return nil, err
		}
	}

	ports, err := s.xrayPorts()
	if err != nil {
		return nil, err
	}

	claimed := make(map[int]chain.Port, len(ports))
	for _, port := range ports {
		claimed[port.Port] = port
	}
	add := func(port chain.Port) error {
		if first, taken := claimed[port.Port]; taken {
			return &ChainError{Code: CodeDuplicatePort, Message: fmt.Sprintf(
				"port %d is claimed twice: %s %q and %s %q",
				port.Port, first.Source, first.Tag, port.Source, port.Tag)}
		}
		claimed[port.Port] = port
		ports = append(ports, port)
		return nil
	}

	tunnels, err := s.tunnelPorts()
	if err != nil {
		return nil, err
	}
	mtproto, err := s.mtprotoPorts()
	if err != nil {
		return nil, err
	}
	rest := append(tunnels, mtproto...)
	for _, port := range extra {
		rest = append(rest, chain.Port{
			Port:    port.Port,
			Network: port.Network,
			Tag:     "extra-" + strconv.Itoa(port.Port),
			Source:  chain.SourceExtra,
		})
	}
	for _, port := range rest {
		if err := add(port); err != nil {
			return nil, err
		}
	}

	sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
	return ports, nil
}

// xrayPorts reads the config xray is running and keeps the inbounds a client
// can actually reach, then puts the public port back where the nginx front end
// took one away.
func (s *ChainPortsService) xrayPorts() ([]chain.Port, error) {
	raw, err := os.ReadFile(s.configPath())
	if err != nil {
		return nil, fmt.Errorf("chain ports: read the xray config %q: %w", s.configPath(), err)
	}
	ports, err := chainports.Build(raw)
	if err != nil {
		return nil, fmt.Errorf("chain ports: %w", err)
	}
	return s.publicPorts(ports)
}

// publicPorts applies the nginx front end (web/service/inbound.go): an inbound
// it moved listens on the loopback under a private port, and the port clients
// — and therefore the fronts — must reach is PublicPort.
//
// The moved inbounds are invisible to chainports, which skips loopback binds,
// so they are added here rather than substituted. Several inbounds share the
// one public port behind nginx, so the result is deduplicated: 443 is relayed
// once, whatever is multiplexed behind it.
func (s *ChainPortsService) publicPorts(ports []chain.Port) ([]chain.Port, error) {
	db := s.handle()
	if db == nil {
		return ports, nil
	}
	var inbounds []model.Inbound
	err := db.Model(&model.Inbound{}).
		Where("enable = ? AND public_port > 0 AND protocol <> ?", true, model.MTProto).
		Order("id").Find(&inbounds).Error
	if err != nil {
		return nil, err
	}

	public := make(map[string]int, len(inbounds))
	for _, inbound := range inbounds {
		public[inbound.Tag] = inbound.PublicPort
	}
	known := make(map[string]bool, len(ports))
	for index := range ports {
		known[ports[index].Tag] = true
		if port, moved := public[ports[index].Tag]; moved {
			ports[index].Port = port
		}
	}
	for _, inbound := range inbounds {
		if known[inbound.Tag] {
			continue
		}
		ports = append(ports, chain.Port{
			Port:    inbound.PublicPort,
			Network: chain.NetworkTCPUDP,
			Tag:     inbound.Tag,
			Source:  chain.SourceXray,
		})
	}

	deduped := make([]chain.Port, 0, len(ports))
	seen := make(map[int]bool, len(ports))
	for _, port := range ports {
		if seen[port.Port] {
			continue
		}
		seen[port.Port] = true
		deduped = append(deduped, port)
	}
	return deduped, nil
}

// tunnelPorts lists the UDP listeners of the enabled AmneziaWG / WireGuard
// servers. They are host listeners, not xray inbounds, so nothing in the xray
// config would ever mention them.
func (s *ChainPortsService) tunnelPorts() ([]chain.Port, error) {
	db := s.handle()
	if db == nil {
		return nil, nil
	}
	var servers []model.TunnelServer
	err := db.Model(&model.TunnelServer{}).
		Where("enable = ? AND listen_port > 0", true).Order("kind").Find(&servers).Error
	if err != nil {
		return nil, err
	}
	ports := make([]chain.Port, 0, len(servers))
	for _, server := range servers {
		source := chain.SourceWg
		if server.Kind == model.TunnelKindAwg {
			source = chain.SourceAwg
		}
		ports = append(ports, chain.Port{
			Port:    server.ListenPort,
			Network: chain.NetworkUDP,
			Tag:     server.Kind,
			Source:  source,
		})
	}
	return ports, nil
}

// mtprotoPorts lists the MTProto inbounds. They are deliberately kept out of
// the xray config (web/service/xray.go — an mtg sidecar serves them), so the
// table is the only place that knows their ports.
func (s *ChainPortsService) mtprotoPorts() ([]chain.Port, error) {
	db := s.handle()
	if db == nil {
		return nil, nil
	}
	var inbounds []model.Inbound
	err := db.Model(&model.Inbound{}).
		Where("enable = ? AND protocol = ?", true, model.MTProto).Order("id").Find(&inbounds).Error
	if err != nil {
		return nil, err
	}
	ports := make([]chain.Port, 0, len(inbounds))
	for index := range inbounds {
		inbound := &inbounds[index]
		if inbound.LinkPort() <= 0 {
			continue
		}
		ports = append(ports, chain.Port{
			Port:    inbound.LinkPort(),
			Network: chain.NetworkTCP,
			Tag:     "mtproto-" + strconv.Itoa(inbound.Id),
			Source:  chain.SourceMtproto,
		})
	}
	return ports, nil
}
