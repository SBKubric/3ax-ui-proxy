package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/util/json_util"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// relayConfigPath is where the generated dokodemo-door relay config is written.
// xray.NewTestProcess removes it again when the relay is stopped.
func relayConfigPath() string {
	return config.GetBinFolderPath() + "/proxy-relay.json"
}

// panelInbound is the minimal shape read from the real panel's xray config.
type panelInbound struct {
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

// tproxyTagSuffix is what the panel names its synthetic transparent-proxy
// inbounds by default ("awg-tproxy-in", "wg-tproxy-in"). Used as a belt to the
// semantic braces below, since the tag is user-editable.
const tproxyTagSuffix = "-tproxy-in"

// transparent reports whether an inbound only receives traffic the kernel
// redirects into it (TPROXY / REDIRECT). The panel adds one per tunnel whose
// traffic is routed via Xray; nothing outside can dial it, so relaying it would
// just open a dead port on the proxy front.
func transparent(in panelInbound) bool {
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

// skipReason explains why a panel inbound is not relayed, or returns "" when it
// should be. Only public-facing ports are relayed: the gRPC api tunnel, loopback
// binds, unix-socket fallbacks and transparent-proxy inbounds are skipped.
func skipReason(in panelInbound) string {
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

// relayable reports whether a panel inbound should be L4-forwarded by the proxy.
func relayable(in panelInbound) bool { return skipReason(in) == "" }

// relayInbound builds one dokodemo-door inbound forwarding port to upstreamHost.
func relayInbound(listenJSON []byte, upstreamHost string, port int, network string) xray.InboundConfig {
	settings := fmt.Sprintf(`{"address":%q,"port":%d,"network":%q,"followRedirect":false}`, upstreamHost, port, network)
	return xray.InboundConfig{
		Listen:   json_util.RawMessage(listenJSON),
		Port:     port,
		Protocol: "dokodemo-door",
		Settings: json_util.RawMessage(settings),
		Tag:      fmt.Sprintf("relay-%d", port),
	}
}

// BuildRelayConfig reads the real panel's exported xray config and builds a
// dokodemo-door relay config that L4-forwards every public inbound port to
// upstreamHost (raw TCP+UDP, so the real server still terminates TLS/Reality and
// no keys live on the proxy), plus every extra port the real server serves
// outside xray (AmneziaWG/WireGuard, MTProto). An extra port that is also an
// xray inbound is rejected: one port, one source. It returns the config plus
// the relayed ports.
func BuildRelayConfig(panelXrayCfgPath, upstreamHost, listen string, extra []ExtraPort) (*xray.Config, []int, error) {
	if listen == "" {
		listen = "::"
	}
	listenJSON, _ := json.Marshal(listen)

	data, err := os.ReadFile(panelXrayCfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read panel xray config %q: %w", panelXrayCfgPath, err)
	}

	var parsed struct {
		Inbounds []panelInbound `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parse panel xray config %q: %w", panelXrayCfgPath, err)
	}

	var inbounds []xray.InboundConfig
	var ports []int
	seen := make(map[int]bool)
	for _, in := range parsed.Inbounds {
		if reason := skipReason(in); reason != "" {
			logger.Infof("proxy-front: not relaying inbound %q (port %d): %s", in.Tag, in.Port, reason)
			continue
		}
		if seen[in.Port] {
			continue
		}
		seen[in.Port] = true
		ports = append(ports, in.Port)
		inbounds = append(inbounds, relayInbound(listenJSON, upstreamHost, in.Port, "tcp,udp"))
	}

	for _, ep := range extra {
		if seen[ep.Port] {
			return nil, nil, fmt.Errorf("extra port %d is already an xray inbound in %q", ep.Port, panelXrayCfgPath)
		}
		seen[ep.Port] = true
		ports = append(ports, ep.Port)
		inbounds = append(inbounds, relayInbound(listenJSON, upstreamHost, ep.Port, ep.Network))
	}

	if len(inbounds) == 0 {
		return nil, nil, fmt.Errorf("no relayable inbounds found in %q and no extraPorts configured", panelXrayCfgPath)
	}

	cfg := &xray.Config{
		LogConfig:       json_util.RawMessage(`{"loglevel":"warning"}`),
		InboundConfigs:  inbounds,
		OutboundConfigs: json_util.RawMessage(`[{"protocol":"freedom","tag":"direct"}]`),
	}
	return cfg, ports, nil
}

// Relay manages the dokodemo-door xray process that forwards traffic upstream.
type Relay struct {
	proc  *xray.Process
	ports []int
}

// NewRelay builds the relay config from cfg and prepares (but does not start) the
// xray process.
func NewRelay(cfg *Config) (*Relay, error) {
	xrayCfg, ports, err := BuildRelayConfig(cfg.XrayConfigPath, cfg.UpstreamHost, cfg.RelayListen, cfg.ExtraRelayPorts())
	if err != nil {
		return nil, err
	}
	return &Relay{
		proc:  xray.NewTestProcess(xrayCfg, relayConfigPath()),
		ports: ports,
	}, nil
}

// Ports returns the inbound ports being relayed.
func (r *Relay) Ports() []int { return r.ports }

// Start launches the relay xray process.
func (r *Relay) Start() error { return r.proc.Start() }

// Stop terminates the relay xray process and removes its config file.
func (r *Relay) Stop() error { return r.proc.Stop() }
