package service

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/chainports"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"

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
// as soon as a build succeeds. An error that is not a ChainError — a failed
// query of the inbounds table, say — is a fault of the panel's own state
// rather than of the port composition, so it clears the banner instead of filling it
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
	mtproto, behindFront, err := s.mtprotoPorts()
	if err != nil {
		return nil, err
	}
	// An MTProto inbound behind the nginx front shares the front's port with
	// whatever else is multiplexed there: nginx tells them apart by SNI, and
	// the fronts relay that port once. It is not a second claim on it (#140).
	for _, port := range behindFront {
		if _, taken := claimed[port.Port]; taken {
			continue
		}
		if err := add(port); err != nil {
			return nil, err
		}
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

// xrayServedProtocols are the inbound protocols xray never sees:
// AmneziaWG/WireGuard are host listeners of their own and MTProto is served by
// an mtg sidecar. XrayService.GetXrayConfig leaves exactly these three out of
// the config it generates, and the ports of the ones that do listen are added
// to the document by tunnelPorts and mtprotoPorts instead.
var notXrayProtocols = []model.Protocol{model.AmneziaWG, model.NativeWG, model.MTProto}

// xrayPorts lists the ports of the inbounds xray serves, straight from the
// panel's own table.
//
// The table, not the generated bin/config.json: the panel rewrites that file
// only when it restarts xray, which happens long after the write that changed
// the ports has bumped the chain's revision. A front asking for the document
// in between would cache the old list under the new revision and nothing would
// ever bump it again (#96). The table is already right when the hook runs —
// the hook shares the write's transaction — so the list is right at the moment
// it is announced.
//
// Which rows land here mirrors XrayService.GetXrayConfig exactly: enabled
// inbounds of a protocol xray serves. The api inbound lives in the config
// template rather than in the table and never appears at all; chainports skips
// it by tag regardless.
func (s *ChainPortsService) xrayPorts() ([]chain.Port, error) {
	db := s.handle()
	if db == nil {
		return nil, nil
	}
	var inbounds []model.Inbound
	err := db.Model(&model.Inbound{}).
		Where("enable = ? AND protocol NOT IN ?", true, notXrayProtocols).
		Order("id").Find(&inbounds).Error
	if err != nil {
		return nil, fmt.Errorf("chain ports: read the inbounds: %w", err)
	}

	served := make([]chainports.Inbound, 0, len(inbounds))
	for index := range inbounds {
		served = append(served, relayedInbound(&inbounds[index]))
	}
	return chainports.Ports(served), nil
}

// relayedInbound states one stored inbound the way the skip rules see it, with
// the nginx front end already applied: an inbound it moved listens on the
// loopback under a private port, and the port clients — and therefore the
// fronts — must reach is PublicPort, on whatever address nginx answers. Left
// as stored, such an inbound would be dropped as a loopback bind and its
// public port would never be relayed.
//
// Several inbounds can share the one public port behind nginx; chainports
// lists a port once, so 443 is relayed once whatever is multiplexed there.
func relayedInbound(inbound *model.Inbound) chainports.Inbound {
	listen := inbound.Listen
	if inbound.PublicPort > 0 {
		listen = ""
	}
	return chainports.Inbound{
		Listen:         listen,
		Port:           inbound.LinkPort(),
		Protocol:       string(inbound.Protocol),
		Tag:            inbound.Tag,
		Settings:       inbound.Settings,
		StreamSettings: inbound.StreamSettings,
	}
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
//
// The ones nginx publishes on its public port come back apart, in
// behindFront: that port is shared by design, so they must not collide with
// the xray inbound multiplexed beside them.
func (s *ChainPortsService) mtprotoPorts() (ports, behindFront []chain.Port, err error) {
	db := s.handle()
	if db == nil {
		return nil, nil, nil
	}
	var inbounds []model.Inbound
	err = db.Model(&model.Inbound{}).
		Where("enable = ? AND protocol = ?", true, model.MTProto).Order("id").Find(&inbounds).Error
	if err != nil {
		return nil, nil, err
	}
	for index := range inbounds {
		inbound := &inbounds[index]
		if inbound.LinkPort() <= 0 {
			continue
		}
		port := chain.Port{
			Port:    inbound.LinkPort(),
			Network: chain.NetworkTCP,
			Tag:     "mtproto-" + strconv.Itoa(inbound.Id),
			Source:  chain.SourceMtproto,
		}
		if inbound.PublicPort > 0 {
			behindFront = append(behindFront, port)
			continue
		}
		ports = append(ports, port)
	}
	return ports, behindFront, nil
}
