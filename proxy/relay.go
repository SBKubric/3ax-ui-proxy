package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/coinman-dev/3ax-ui/v2/chain"
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

// PortsHint is appended to every rejection so the operator knows where the
// port list comes from now that no manifest exists (docs/spec/proxy-chain.md
// §5.9): the panel computes it, and `x-ui chain ports` prints exactly what a
// chain document carries.
const PortsHint = "the panel computes the relayed ports — print them there with `x-ui chain ports` and use that output"

// ParseRelayPorts reads the relayed-port list the front works from: the same
// array of ports a chain document carries (§3.8), so the box has one format
// whether the list arrived in a document or was pasted by the operator.
//
// It is strict about the vocabulary rather than about the shape: a port the
// relay cannot open, or a network it cannot name, is a silently dead listener
// otherwise.
func ParseRelayPorts(data []byte) ([]chain.Port, error) {
	var ports []chain.Port
	if err := json.Unmarshal(data, &ports); err != nil {
		return nil, fmt.Errorf("not a chain port list (%v); %s", err, PortsHint)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("the chain port list is empty; %s", PortsHint)
	}
	for _, port := range ports {
		if port.Port < 1 || port.Port > 65535 {
			return nil, fmt.Errorf("port %d is out of range 1-65535; %s", port.Port, PortsHint)
		}
		switch port.Network {
		case chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP:
		default:
			return nil, fmt.Errorf("port %d has network %q, want one of %q, %q, %q; %s",
				port.Port, port.Network, chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP, PortsHint)
		}
	}
	return ports, nil
}

// LoadRelayPorts reads the port list from disk.
func LoadRelayPorts(path string) ([]chain.Port, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read chain port list %q: %w", path, err)
	}
	ports, err := ParseRelayPorts(data)
	if err != nil {
		return nil, fmt.Errorf("chain port list %q: %w", path, err)
	}
	return ports, nil
}

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

// BuildRelayConfig builds a dokodemo-door relay config that L4-forwards every
// port of the chain document to upstreamHost (raw TCP and/or UDP, so the real
// server still terminates TLS/Reality and no keys live on the front).
//
// The list already carries every source the panel knows — xray inbounds,
// AmneziaWG/WireGuard, MTProto and the operator's extras — so this no longer
// decides what is relayable; it only turns ports into listeners. One port, one
// source still holds: a port claimed twice is a refusal, not a silent winner.
// It returns the config plus the relayed ports.
func BuildRelayConfig(ports []chain.Port, upstreamHost, listen string) (*xray.Config, []int, error) {
	if listen == "" {
		listen = "::"
	}
	listenJSON, _ := json.Marshal(listen)

	var inbounds []xray.InboundConfig
	var relayed []int
	seen := make(map[int]chain.Port, len(ports))
	for _, port := range ports {
		if first, taken := seen[port.Port]; taken {
			return nil, nil, fmt.Errorf("port %d is declared twice: %s %q and %s %q",
				port.Port, first.Source, first.Tag, port.Source, port.Tag)
		}
		seen[port.Port] = port
		relayed = append(relayed, port.Port)
		inbounds = append(inbounds, relayInbound(listenJSON, upstreamHost, port.Port, port.Network))
	}

	if len(inbounds) == 0 {
		return nil, nil, fmt.Errorf("the chain port list carries no ports; %s", PortsHint)
	}

	cfg := &xray.Config{
		LogConfig:       json_util.RawMessage(`{"loglevel":"warning"}`),
		InboundConfigs:  inbounds,
		OutboundConfigs: json_util.RawMessage(`[{"protocol":"freedom","tag":"direct"}]`),
	}
	return cfg, relayed, nil
}

// extraRelayPorts turns the box-local extra ports of proxy.json into document
// ports, so BuildRelayConfig sees one list. They keep the document's own
// naming for an operator port (§3.8).
func extraRelayPorts(extra []ExtraPort) []chain.Port {
	ports := make([]chain.Port, 0, len(extra))
	for _, ep := range extra {
		ports = append(ports, chain.Port{
			Port:    ep.Port,
			Network: ep.Network,
			Tag:     "extra-" + strconv.Itoa(ep.Port),
			Source:  chain.SourceExtra,
		})
	}
	return ports
}

// Relay manages the dokodemo-door xray process that forwards traffic upstream.
type Relay struct {
	proc  *xray.Process
	ports []int
}

// NewRelay builds the relay config from cfg and prepares (but does not start) the
// xray process.
func NewRelay(cfg *Config) (*Relay, error) {
	ports, err := LoadRelayPorts(cfg.RelayManifestPath)
	if err != nil {
		return nil, err
	}
	ports = append(ports, extraRelayPorts(cfg.ExtraRelayPorts())...)
	xrayCfg, relayed, err := BuildRelayConfig(ports, cfg.UpstreamHost, cfg.RelayListen)
	if err != nil {
		return nil, err
	}
	logger.Infof("proxy-front: relaying %d ports to %s", len(relayed), cfg.UpstreamHost)
	return &Relay{
		proc:  xray.NewTestProcess(xrayCfg, relayConfigPath()),
		ports: relayed,
	}, nil
}

// Ports returns the inbound ports being relayed.
func (r *Relay) Ports() []int { return r.ports }

// Start launches the relay xray process.
func (r *Relay) Start() error { return r.proc.Start() }

// Stop terminates the relay xray process and removes its config file.
func (r *Relay) Stop() error { return r.proc.Stop() }
