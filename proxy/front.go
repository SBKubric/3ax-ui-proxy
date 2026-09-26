package proxy

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

// frontPort is the one TCP port a box answers on while its front is up (ADR
// 0005). Not a setting, for the same reason as on the panel: a website lives
// on 443.
const frontPort = 443

// acmePort is nginx's other port on every box: the ACME webroot the IP
// certificate renews through (x-ui nginx acme-front).
const acmePort = 80

// The range the front's loopback listeners are picked from: the same one the
// panel uses, above the well-known ports and below the ephemeral ones.
const (
	frontLoopbackFirst = 8081
	frontLoopbackLast  = 8999
)

// FrontLayout is where the pieces of a box's front meet on the loopback.
//
// There is no settings table on a box to remember a choice in, so the layout
// is a function of the document instead: the same document always gives the
// same ports, and a config that did not change does not reload nginx.
type FrontLayout struct {
	// SiteListen is where the HTTP side terminates TLS for requests by
	// address and serves the decoy.
	SiteListen string `json:"siteListen"`
	// SubListen is the box's sub server behind the HTTP side, plain HTTP:
	// nginx has already terminated TLS.
	SubListen string `json:"subListen"`
	// NextHopRelay and TargetRelay take the PROXY header back off before a
	// stream leaves the box raw — to the next hop's 443, or to an edge's
	// neighbour target. Another box's front reads the SNI from the first
	// bytes it receives, and a site on the internet has never heard of the
	// header.
	NextHopRelay string `json:"nextHopRelay"`
	TargetRelay  string `json:"targetRelay"`
}

// NewFrontLayout picks the loopback ports for a box whose relay carries doc's
// ports and whose sub server listened on subPort before the front. Both are
// avoided: the relay binds its ports on every address, loopback included, and
// the old sub port stays up while the outer neighbours move (§ the sub-port
// transition in docs/spec/proxy-chain.md).
func NewFrontLayout(doc *chain.Document, subPort int) FrontLayout {
	taken := map[int]bool{subPort: true, frontPort: true, acmePort: true}
	if doc != nil {
		for _, port := range doc.Ports {
			taken[port.Port] = true
		}
	}
	next := frontLoopbackFirst
	pick := func() string {
		for next <= frontLoopbackLast && taken[next] {
			next++
		}
		taken[next] = true
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(next))
	}
	return FrontLayout{SiteListen: pick(), SubListen: pick(), NextHopRelay: pick(), TargetRelay: pick()}
}

// BuildFront turns a chain document into the box's nginx config (ADR 0005,
// #140). The split by SNI depends on the role:
//
//   - an edge passes its own neighbour target's server name — the one the
//     chain-following inbounds accept while it is active — raw to the next
//     hop, and an unknown name raw to the neighbour target itself, so a
//     prober sees the neighbour's site exactly as an unauthenticated Reality
//     client does;
//   - an inner passes the active edge's server name raw to the next hop and
//     gives an unknown one the decoy: there is no Reality here to answer it;
//   - a request by address, without SNI, is the HTTP side on either: the IP
//     certificate, the box's own sub, json, wave and join paths in front of
//     the sub server, and the decoy page for everything else.
//
// It returns the warnings the owner has to hear — a front that routes no
// client at all is legal but almost certainly not what was meant.
func BuildFront(doc *chain.Document, layout FrontLayout, ipCert, ipKey string) (nginx.Config, []string, error) {
	cfg := nginx.Config{Mode: nginx.ModeOnly443, Port: frontPort}
	if doc == nil {
		return cfg, nil, errors.New("the front needs a chain document: nothing says where to pass the clients on to")
	}
	if ipCert == "" || ipKey == "" {
		return cfg, nil, errors.New("the front needs the box's IP certificate: behind it the sub server is reachable by address only")
	}

	var warnings []string
	nextHop := net.JoinHostPort(doc.NextHop.Host, strconv.Itoa(frontPort))
	switch doc.Self.Role {
	case chain.RoleEdge:
		if doc.Self.RealityServerName == "" || doc.Self.RealityTarget == "" {
			warnings = append(warnings, fmt.Sprintf(
				"edge %s has no neighbour target in the chain document: an unknown SNI gets the decoy page, and no client SNI is passed on until the panel sets one",
				doc.Self.Name))
			break
		}
		cfg.Routes = append(cfg.Routes,
			nginx.Route{
				Name:     "clients of " + doc.Self.Name + " → next hop",
				SNIs:     []string{doc.Self.RealityServerName},
				Upstream: nextHop,
				Relay:    layout.NextHopRelay,
				Raw:      true,
			},
			nginx.Route{
				Name:     "neighbour target of " + doc.Self.Name,
				Upstream: doc.Self.RealityTarget,
				Relay:    layout.TargetRelay,
				Raw:      true,
				Fallback: true,
			})
	default:
		name := activeEdgeServerName(doc)
		if name == "" {
			warnings = append(warnings, fmt.Sprintf(
				"inner %s knows no server name of an active edge: no client SNI is passed on, everything but the HTTP side gets the decoy page",
				doc.Self.Name))
			break
		}
		cfg.Routes = append(cfg.Routes, nginx.Route{
			Name:     "clients of the active edge " + doc.ActiveEdge + " → next hop",
			SNIs:     []string{name},
			Upstream: nextHop,
			Relay:    layout.NextHopRelay,
			Raw:      true,
		})
	}

	cfg.Site = &nginx.Site{
		IPCertFile: ipCert,
		IPKeyFile:  ipKey,
		Listen:     layout.SiteListen,
		Root:       nginx.WebRoot,
		Sub: &nginx.Proxy{
			Name:   "sub server of " + doc.Self.Name,
			Paths:  frontSubPaths(doc),
			Target: layout.SubListen,
		},
	}
	return cfg, warnings, nil
}

// activeEdgeServerName is the server name clients of the active edge send,
// from that edge's entry in the hops list.
func activeEdgeServerName(doc *chain.Document) string {
	if doc.ActiveEdge == "" {
		return ""
	}
	for _, hop := range doc.Hops {
		if hop.Name == doc.ActiveEdge {
			return hop.RealityServerName
		}
	}
	return ""
}

// frontSubPaths is what the HTTP side passes to the box's sub server: the
// subscription paths of the document, the wave, and the join page.
func frontSubPaths(doc *chain.Document) []string {
	seen := map[string]bool{}
	var paths []string
	for _, path := range []string{doc.NextHop.SubPath, doc.NextHop.JsonPath, ChainPathPrefix + "/", "/join/"} {
		if path == "" || path == "/" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// RelayPorts is what the dokodemo relay carries with the front up or down.
//
// Behind the front nginx owns 443/tcp and the relay must leave it alone.
// Every UDP port — 443/udp included — is relayed exactly as before: nginx
// has no say over UDP. A TCP port other than 443 is not relayed at all: the
// front exists so that a box answers on 443 only, and relaying a second TCP
// port would put back what it closed. dropped names those ports, 443 not
// among them, for the log line the owner reads.
func RelayPorts(ports []chain.Port, frontOn bool) (kept []chain.Port, dropped []int) {
	if !frontOn {
		return ports, nil
	}
	for _, port := range ports {
		switch port.Network {
		case chain.NetworkUDP:
			kept = append(kept, port)
		case chain.NetworkTCPUDP:
			port.Network = chain.NetworkUDP
			kept = append(kept, port)
			if port.Port != frontPort {
				dropped = append(dropped, port.Port)
			}
		default:
			if port.Port != frontPort {
				dropped = append(dropped, port.Port)
			}
		}
	}
	return kept, dropped
}

// FrontFirewall is what stays reachable on a box with its front up (#140):
// 443 for the front, 80 for the ACME webroot the IP certificate renews
// through, the relayed UDP ports, and oldSubPort — the sub port outer
// neighbours polled before the front — while one of them may still be on it
// (0 once they have all moved). SSH is added by nginx.ApplyFirewall itself.
func FrontFirewall(relayed []chain.Port, oldSubPort int) nginx.Firewall {
	fw := nginx.Firewall{TCP: nginx.Ports(frontPort, acmePort)}
	if oldSubPort > 0 {
		fw.TCP = append(fw.TCP, nginx.Port(oldSubPort))
	}
	for _, port := range relayed {
		if port.Network == chain.NetworkUDP || port.Network == chain.NetworkTCPUDP {
			fw.UDP = append(fw.UDP, nginx.Port(port.Port))
		}
	}
	return fw
}

// directOuterNeighbours names the hops that poll this one, out of its own
// document. The panel lists the inner path in order and the edges after it
// (§3.2), and edges hang off the last inner, so the hops polling this inner
// are the ones after it up to and including the first inner that is not on
// its way out — or, when no such inner follows, every hop after it. A
// draining inner stays where it was and keeps polling until its neighbours
// have re-chained past it (§4.5), so it counts too. An edge has none.
func directOuterNeighbours(doc *chain.Document) []string {
	if doc == nil || doc.Self.Role == chain.RoleEdge {
		return nil
	}
	var names []string
	outward := false
	for _, hop := range doc.Hops {
		if hop.Name == doc.Self.Name {
			outward = true
			continue
		}
		if !outward {
			continue
		}
		names = append(names, hop.Name)
		if hop.Role == chain.RoleInner && hop.State != chain.StateDraining {
			break
		}
	}
	return names
}

// OldSubPortNeeded is the rule of the sub-port move (#140): whether the sub
// port this box served before its front came up still has to answer.
//
// The front came up while the box was on revision since. Every document it
// hands out from then on sends its outer neighbours to 443, and the panel
// bumps the revision as soon as it hears the front report, so a neighbour
// that acknowledges a revision newer than since has applied one of those
// documents and polls through the front. Until every direct outer neighbour
// has, the old port stays: the poll that carries the news arrives on it. An
// edge has nobody polling it and needs it no longer at once.
//
// A neighbour that never acknowledges — a dead box still in the registry —
// keeps the old port open. That is the safe way to be wrong: an open port
// the firewall still lists, not a neighbour cut off from the wave.
func OldSubPortNeeded(doc *chain.Document, since int64, acks []OuterAck) bool {
	seen := make(map[string]int64, len(acks))
	for _, ack := range acks {
		seen[ack.Name] = ack.LastRevision
	}
	for _, name := range directOuterNeighbours(doc) {
		if seen[name] <= since {
			return true
		}
	}
	return false
}
