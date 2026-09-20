// Package chainports turns the panel's generated xray config into the xray
// half of the chain document's relayed-port list (docs/spec/proxy-chain.md
// §3.8, §5.9).
//
// It is what is left of the old relaymanifest package: no manifest travels
// any more — the panel computes the ports itself and the chain document
// carries them — so only the part that decides which inbound is worth a port
// on a front survives, and it survives unchanged. The other sources of ports
// (AmneziaWG/WireGuard, MTProto, the operator's extra list) do not live in the
// xray config and are added by web/service.
//
// Like package chain, this package knows nothing of the panel: it takes bytes
// and returns wire types.
package chainports

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// tproxyTagSuffix is what the panel names its synthetic transparent-proxy
// inbounds by default ("awg-tproxy-in", "wg-tproxy-in"). Used as a belt to the
// semantic braces below, since the tag is user-editable.
const tproxyTagSuffix = "-tproxy-in"

// inbound is the part of one xray inbound that decides whether it becomes a
// relayed port. It is deliberately internal: nothing outside needs the xray
// config's shape, only the ports that come out of it.
type inbound struct {
	Listen   string `json:"listen"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Tag      string `json:"tag"`
	Settings struct {
		FollowRedirect bool `json:"followRedirect"`
	} `json:"settings"`
	StreamSettings struct {
		Sockopt struct {
			Tproxy string `json:"tproxy"`
		} `json:"sockopt"`
	} `json:"streamSettings"`
}

// transparent reports whether an inbound only receives traffic the kernel
// redirects into it (TPROXY / REDIRECT). The panel adds one per tunnel whose
// traffic is routed via Xray; nothing outside can dial it, so relaying it would
// just open a dead port on the front.
func transparent(in inbound) bool {
	if in.Protocol == "dokodemo-door" {
		switch strings.ToLower(in.StreamSettings.Sockopt.Tproxy) {
		case "tproxy", "redirect":
			return true
		}
		if in.Settings.FollowRedirect {
			return true
		}
	}
	return strings.HasSuffix(in.Tag, tproxyTagSuffix)
}

// skipReason explains why an inbound gets no relayed port, or returns "" when
// it should have one. Only public-facing ports are relayed: the gRPC api
// tunnel, loopback binds, unix-socket fallbacks and transparent-proxy inbounds
// are skipped. These five rules are the one piece of the relay manifest that
// the chain inherits verbatim (§5.9).
func skipReason(in inbound) string {
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

// Build extracts the relayed ports of a raw xray config (the panel's
// bin/config.json — what xray actually runs, so a port that is there is a port
// clients can reach). Every port carries both networks: the relay opens one
// dokodemo-door for TCP and UDP alike, because the panel's own inbound may use
// either and the front cannot tell from the config which one matters.
//
// A port claimed twice inside one config is listed once: xray could not bind
// it twice either.
func Build(rawXrayConfig []byte) ([]chain.Port, error) {
	var src struct {
		Inbounds []inbound `json:"inbounds"`
	}
	if err := json.Unmarshal(rawXrayConfig, &src); err != nil {
		return nil, fmt.Errorf("parse xray config: %w", err)
	}

	ports := make([]chain.Port, 0, len(src.Inbounds))
	seen := make(map[int]bool, len(src.Inbounds))
	for _, in := range src.Inbounds {
		if skipReason(in) != "" {
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
	return ports, nil
}
