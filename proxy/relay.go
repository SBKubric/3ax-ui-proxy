package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/config"
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
}

// relayable reports whether a panel inbound should be L4-forwarded by the proxy.
// Internal inbounds (the gRPC api tunnel, loopback binds, unix-socket fallbacks)
// are skipped — only public-facing ports are relayed.
func relayable(in panelInbound) bool {
	if in.Port <= 0 || in.Tag == "api" {
		return false
	}
	listen := strings.TrimSpace(in.Listen)
	switch listen {
	case "127.0.0.1", "::1", "localhost":
		return false
	}
	if strings.HasPrefix(listen, "@") { // unix-socket fallback master
		return false
	}
	return true
}

// BuildRelayConfig reads the real panel's exported xray config and builds a
// dokodemo-door relay config that L4-forwards every public inbound port to
// upstreamHost (raw TCP+UDP, so the real server still terminates TLS/Reality and
// no keys live on the proxy). It returns the config plus the relayed ports.
func BuildRelayConfig(panelXrayCfgPath, upstreamHost string) (*xray.Config, []int, error) {
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
		if !relayable(in) || seen[in.Port] {
			continue
		}
		seen[in.Port] = true
		ports = append(ports, in.Port)

		settings := fmt.Sprintf(`{"address":%q,"port":%d,"network":"tcp,udp","followRedirect":false}`, upstreamHost, in.Port)
		inbounds = append(inbounds, xray.InboundConfig{
			Listen:   json_util.RawMessage(`"0.0.0.0"`),
			Port:     in.Port,
			Protocol: "dokodemo-door",
			Settings: json_util.RawMessage(settings),
			Tag:      fmt.Sprintf("relay-%d", in.Port),
		})
	}

	if len(inbounds) == 0 {
		return nil, nil, fmt.Errorf("no relayable inbounds found in %q", panelXrayCfgPath)
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
	xrayCfg, ports, err := BuildRelayConfig(cfg.XrayConfigPath, cfg.UpstreamHost)
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
