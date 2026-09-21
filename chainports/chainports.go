// Package chainports decides which of the panel's inbounds is worth a relayed
// port on a front, and turns the survivors into the xray half of the chain
// document's port list (docs/spec/proxy-chain.md §3.8, §5.9).
//
// It is what is left of the old relay manifest: no manifest travels any more —
// the panel computes the ports itself and the chain document carries them — so
// only the rules that pick the reachable inbounds survive, and they survive
// unchanged. The other sources of ports (AmneziaWG/WireGuard, MTProto, the
// operator's extra list) are not inbounds at all and are added by web/service.
//
// The inbounds come from the panel's own table rather than from the generated
// bin/config.json: that file is rewritten only when xray restarts, which is
// long after the revision that announces the change has been bumped, so a
// front would cache yesterday's ports under today's revision and nothing would
// ever bump again. The table is right at the moment of the bump.
//
// Like package chain, this package knows nothing of the panel: it takes plain
// values and returns wire types. Which rows of the table become an Inbound —
// enabled, and of a protocol xray actually serves — is web/service's business,
// because only it knows how the xray config is generated.
package chainports

import (
	"encoding/json"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// tproxyTagSuffix is what the panel names its synthetic transparent-proxy
// inbounds by default ("awg-tproxy-in", "wg-tproxy-in"). Used as a belt to the
// semantic braces below, since the tag is user-editable.
const tproxyTagSuffix = "-tproxy-in"

// Inbound is the part of one inbound that decides whether it becomes a relayed
// port: the address it answers on and the two JSON blobs that can say it only
// ever hears from the kernel. Settings and StreamSettings are the raw JSON the
// panel stores (and writes into the xray config verbatim); an empty or
// unparsable blob simply tells us nothing.
type Inbound struct {
	Listen         string
	Port           int
	Protocol       string
	Tag            string
	Settings       string
	StreamSettings string
}

// followRedirect reports whether the inbound reads its destination from a
// netfilter REDIRECT rather than from the client.
func followRedirect(in Inbound) bool {
	var parsed struct {
		FollowRedirect bool `json:"followRedirect"`
	}
	if strings.TrimSpace(in.Settings) == "" {
		return false
	}
	if err := json.Unmarshal([]byte(in.Settings), &parsed); err != nil {
		return false
	}
	return parsed.FollowRedirect
}

// sockoptTproxy returns the inbound's streamSettings.sockopt.tproxy mode, or ""
// when it has none.
func sockoptTproxy(in Inbound) string {
	var parsed struct {
		Sockopt struct {
			Tproxy string `json:"tproxy"`
		} `json:"sockopt"`
	}
	if strings.TrimSpace(in.StreamSettings) == "" {
		return ""
	}
	if err := json.Unmarshal([]byte(in.StreamSettings), &parsed); err != nil {
		return ""
	}
	return parsed.Sockopt.Tproxy
}

// transparent reports whether an inbound only receives traffic the kernel
// redirects into it (TPROXY / REDIRECT). The panel adds one per tunnel whose
// traffic is routed via Xray; nothing outside can dial it, so relaying it would
// just open a dead port on the front.
func transparent(in Inbound) bool {
	if in.Protocol == "dokodemo-door" {
		switch strings.ToLower(sockoptTproxy(in)) {
		case "tproxy", "redirect":
			return true
		}
		if followRedirect(in) {
			return true
		}
	}
	return strings.HasSuffix(in.Tag, tproxyTagSuffix)
}

// SkipReason explains why an inbound gets no relayed port, or returns "" when
// it should have one. Only public-facing ports are relayed: the gRPC api
// tunnel, loopback binds, unix-socket fallbacks and transparent-proxy inbounds
// are skipped. These five rules are the one piece of the relay manifest that
// the chain inherits verbatim (§5.9).
func SkipReason(in Inbound) string {
	if in.Port <= 0 {
		return "no port"
	}
	if in.Tag == "api" {
		return "internal api inbound"
	}
	listen := strings.TrimSpace(in.Listen)
	switch listen {
	case "127.0.0.1", "::1", "localhost":
		return "loopback bind"
	}
	if strings.HasPrefix(listen, "@") { // unix-socket fallback master
		return "unix-socket fallback"
	}
	if transparent(in) {
		return "transparent-proxy (TPROXY) inbound"
	}
	return ""
}

// Ports keeps the inbounds a client can actually reach and states them in the
// document's vocabulary. Every port carries both networks: the relay opens one
// dokodemo-door for TCP and UDP alike, because the panel's own inbound may use
// either and the front cannot tell which one matters.
//
// A port claimed twice — several inbounds multiplexed behind one nginx front
// end, say — is listed once: the front can only relay it once either.
func Ports(inbounds []Inbound) []chain.Port {
	ports := make([]chain.Port, 0, len(inbounds))
	seen := make(map[int]bool, len(inbounds))
	for _, in := range inbounds {
		if SkipReason(in) != "" {
			continue
		}
		if seen[in.Port] {
			continue
		}
		seen[in.Port] = true
		ports = append(ports, chain.Port{
			Port:    in.Port,
			Network: chain.NetworkTCPUDP,
			Tag:     in.Tag,
			Source:  chain.SourceXray,
		})
	}
	return ports
}
