package service

import (
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// CodePanelHostUnset refuses to build a document for a chain whose innermost
// hop has nowhere to dial: the panel's own address (§3.2).
const CodePanelHostUnset = "panel_host_unset"

// ChainDocumentService builds the chain document — the truncated view of the
// chain one hop receives from its next hop (docs/spec/proxy-chain.md §3.1,
// §3.2).
//
// Truncation is the whole point of the type: a document carries the hop
// itself, everything outward from it, and of everything inward exactly one
// address — nextHop.host. A seized box therefore gives away its own secret and
// the address of its neighbour, and nothing else about the chain.
type ChainDocumentService struct {
	settingService SettingService
	portsService   ChainPortsService
}

// ETag is the HTTP validator of a document: the chain's revision, quoted
// (§3.4). One string, built in one place, because the wave's 304 depends on
// the panel and the box spelling it the same way.
func ETag(revision int64) string {
	return `"` + strconv.FormatInt(revision, 10) + `"`
}

// Build returns the document for the hop named forHopName, with no fallback
// for the panel's own address: only chainPanelHost can answer it.
func (s *ChainDocumentService) Build(forHopName string) (*chain.Document, error) {
	return s.BuildWithPanelHost(forHopName, "")
}

// BuildWithPanelHost is Build with the address the caller was reached at.
//
// The panel cannot work out its own reachable address: what it knows is a
// listen address, and behind NAT, a tunnel or a reverse proxy that is not what
// a front dials. Usually it does not have to — the first-tier hop just polled
// it, so the Host of that request is an address that demonstrably works, and
// #81 passes it here. chainPanelHost overrides it for the cases where the
// request cannot be trusted to carry it: a front reaching the panel through
// something that rewrites Host, or a document built by the UI with no request
// in hand at all.
func (s *ChainDocumentService) BuildWithPanelHost(forHopName, fallbackHost string) (*chain.Document, error) {
	documents, err := s.BuildAllWithPanelHost(fallbackHost)
	if err != nil {
		return nil, err
	}
	document, found := documents[forHopName]
	if !found {
		return nil, chainErrorf(CodeUnknownHop, "no hop named %q in the chain", forHopName)
	}
	return document, nil
}

// BuildAll returns the document of every hop that has entered the chain,
// keyed by hop name. The wave serves one hop at a time, but the ports, the
// revision and the sub settings are the same for all of them, so building the
// set together is both cheaper and the only way the documents are guaranteed
// to agree with each other.
//
// A brand-new pending hop appears nowhere: not as a document of its own, not
// in anyone's hops list, and not as anyone's next hop (§2.6.3). The registry
// re-chains next_hop_id onto a new inner the moment it is created, so a hop's
// next hop here is found by walking inward past every such hop — until one
// that is visible, or the panel itself.
//
// Two states are visible although they are not in the live path. A re-entering
// hop (pending with a secret hash, §4.5.7) stays exactly where it was: its box
// is alive and is still serving its neighbours until the new one enters. A
// draining hop (§4.5.2) keeps its own document and stays in the hops[] of its
// next hop — the single place its secret hash still lives, and what makes it
// authenticable while it hands its neighbours over.
func (s *ChainDocumentService) BuildAll() (map[string]*chain.Document, error) {
	return s.BuildAllWithPanelHost("")
}

// BuildAllWithPanelHost is BuildAll with the address the caller was reached
// at, used for the panel's own host when chainPanelHost is unset
// (BuildWithPanelHost explains why).
func (s *ChainDocumentService) BuildAllWithPanelHost(fallbackHost string) (map[string]*chain.Document, error) {
	// Settings before anything else: SQLite runs on one connection here, so
	// reading them later, from inside a query, would wait on itself.
	revision, err := s.settingService.GetChainRevision()
	if err != nil {
		return nil, err
	}
	panelHop, err := s.panelAsNextHop(fallbackHost)
	if err != nil {
		return nil, err
	}

	hops, err := orderedHops(database.GetDB())
	if err != nil {
		return nil, err
	}
	entered := make([]model.ChainHop, 0, len(hops))
	byId := make(map[int]model.ChainHop, len(hops))
	for _, hop := range hops {
		byId[hop.Id] = hop
		if chainHopVisible(hop) {
			entered = append(entered, hop)
		}
	}
	if len(entered) == 0 {
		return map[string]*chain.Document{}, nil
	}
	if panelHop.Host == "" {
		return nil, chainErrorf(CodePanelHostUnset,
			"the chain has hops but the panel has no address: set chainPanelHost, or let the hop's own request supply one")
	}

	ports, err := s.portsService.Ports()
	if err != nil {
		return nil, err
	}

	// entered is ordered inner path first (by position), then the edges, so
	// "outward from H" is a suffix of it for an inner and H alone for an edge.
	wire := make([]chain.Hop, 0, len(entered))
	for _, hop := range entered {
		wire = append(wire, chain.Hop{
			Name:       hop.Name,
			Role:       hop.Role,
			Host:       hop.Host,
			SubPort:    hop.SubPort,
			SecretHash: hop.SecretHash,
			State:      hop.State,

			RealityTarget:     hop.RealityTarget,
			RealityServerName: hop.NeighbourServerName(),
		})
	}
	activeEdge := ""
	for _, hop := range entered {
		if hop.IsActive && hop.Role == chain.RoleEdge {
			activeEdge = hop.Name
			break
		}
	}

	generatedAt := time.Now().UnixMilli()
	documents := make(map[string]*chain.Document, len(entered))
	for index, hop := range entered {
		document := &chain.Document{
			Version:     chain.DocumentVersion,
			Revision:    revision,
			GeneratedAt: generatedAt,
			Self: chain.Self{Name: hop.Name, Role: hop.Role, Host: hop.Host, State: hop.State,
				RealityTarget: hop.RealityTarget, RealityServerName: hop.NeighbourServerName()},
			NextHop: s.nextHopOf(hop, byId, panelHop),
			Ports:   ports,
		}
		if hop.Role == chain.RoleEdge {
			// Outward of an edge there are only clients, and the edge beside
			// it is not outward but sideways: a seized standby must not learn
			// that its neighbour exists, let alone that it is the active one.
			document.Hops = []chain.Hop{wire[index]}
			if hop.IsActive {
				document.ActiveEdge = activeEdge
			}
		} else {
			document.Hops = append([]chain.Hop(nil), wire[index:]...)
			document.ActiveEdge = activeEdge
		}
		documents[hop.Name] = document
	}
	return documents, nil
}

// nextHopOf resolves what the hop dials inward. A brand-new pending hop in
// between is skipped rather than named: its box does not exist yet, and
// pointing a live front at it would cut the chain until someone installed it
// (§2.6.3). A draining hop is skipped for the opposite reason: it is on its
// way out, so the live path runs past it (§4.5.2) — the departing box hands
// its neighbours this very address itself, out of its own document (§4.5.3).
func (s *ChainDocumentService) nextHopOf(hop model.ChainHop, byId map[int]model.ChainHop, panelHop chain.NextHop) chain.NextHop {
	for id := hop.NextHopId; id != nil; {
		next, found := byId[*id]
		if !found {
			break
		}
		if chainHopVisible(next) && next.State != chain.StateDraining {
			return chain.NextHop{
				Host:      next.Host,
				SubPort:   next.SubPort,
				SubScheme: next.SubScheme,
				SubPath:   panelHop.SubPath,
				JsonPath:  panelHop.JsonPath,
				TunPath:   panelHop.TunPath,
			}
		}
		id = next.NextHopId
	}
	return panelHop
}

// chainHopVisible reports whether a registry row takes part in the documents
// at all. Everything does except a hop that was created and never entered:
// that one has no box, so naming it anywhere would point a live front at
// nothing (§2.6.3). A re-entering hop is told apart by its secret hash, which
// only a hop that has already entered can have (§4.5.7).
func chainHopVisible(hop model.ChainHop) bool {
	return hop.State != chain.StatePending || hop.SecretHash != ""
}

// panelAsNextHop is what the innermost hop dials: the panel itself. The host
// is chainPanelHost when the owner stated one and the caller's own view of the
// panel otherwise; the paths are the panel's real subscription paths, which
// travel in the document so no box has to store them.
func (s *ChainDocumentService) panelAsNextHop(fallbackHost string) (chain.NextHop, error) {
	host, err := s.settingService.GetChainPanelHost()
	if err != nil {
		return chain.NextHop{}, err
	}
	if host == "" {
		host = strings.TrimSpace(fallbackHost)
	}
	subPort, err := s.settingService.GetSubPort()
	if err != nil {
		return chain.NextHop{}, err
	}
	subPath, err := s.settingService.GetSubPath()
	if err != nil {
		return chain.NextHop{}, err
	}
	jsonPath, err := s.settingService.GetSubJsonPath()
	if err != nil {
		return chain.NextHop{}, err
	}
	scheme := "http"
	certFile, err := s.settingService.GetSubCertFile()
	if err != nil {
		return chain.NextHop{}, err
	}
	keyFile, err := s.settingService.GetSubKeyFile()
	if err != nil {
		return chain.NextHop{}, err
	}
	if certFile != "" && keyFile != "" {
		scheme = "https"
	}
	// With nginx in front of the subscriptions they live on the public port
	// under its own certificate, and that is what a front must dial.
	if publicScheme, _, ok := PublicSubBase(); ok {
		scheme = publicScheme
		subPort = PublicPort
	}
	return chain.NextHop{
		Host:      host,
		SubPort:   subPort,
		SubScheme: scheme,
		SubPath:   subPath,
		JsonPath:  jsonPath,
	}, nil
}
