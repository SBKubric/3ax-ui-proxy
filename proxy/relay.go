package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

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
// §5.9): the panel computes it, the chain document carries it, and `x-ui chain
// ports` on the panel prints exactly what a document carries.
const PortsHint = "the panel computes the relayed ports — print them there with `x-ui chain ports`"

// relayInbound builds one dokodemo-door inbound forwarding port to nextHopHost.
func relayInbound(listenJSON []byte, nextHopHost string, port int, network string) xray.InboundConfig {
	settings := fmt.Sprintf(`{"address":%q,"port":%d,"network":%q,"followRedirect":false}`, nextHopHost, port, network)
	return xray.InboundConfig{
		Listen:   json_util.RawMessage(listenJSON),
		Port:     port,
		Protocol: "dokodemo-door",
		Settings: json_util.RawMessage(settings),
		Tag:      fmt.Sprintf("relay-%d", port),
	}
}

// BuildRelayConfig builds a dokodemo-door relay config that L4-forwards every
// port of the chain document to nextHopHost (raw TCP and/or UDP, so the real
// server still terminates TLS/Reality and no keys live on a hop).
//
// The document's list already carries every source the panel knows — xray
// inbounds, AmneziaWG/WireGuard, MTProto and the operator's extras — so this
// does not decide what is relayable; it only turns ports into listeners. One
// port, one source still holds: a port claimed twice is a refusal, not a
// silent winner. It returns the config plus the relayed ports.
func BuildRelayConfig(ports []chain.Port, nextHopHost, listen string) (*xray.Config, []int, error) {
	if listen == "" {
		listen = DefaultRelayListen
	}
	listenJSON, _ := json.Marshal(listen)

	var inbounds []xray.InboundConfig
	var relayed []int
	seen := make(map[int]chain.Port, len(ports))
	for _, port := range ports {
		if port.Port < 1 || port.Port > 65535 {
			return nil, nil, fmt.Errorf("port %d is out of range 1-65535; %s", port.Port, PortsHint)
		}
		switch port.Network {
		case chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP:
		default:
			return nil, nil, fmt.Errorf("port %d has network %q, want one of %q, %q, %q; %s",
				port.Port, port.Network, chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP, PortsHint)
		}
		if first, taken := seen[port.Port]; taken {
			return nil, nil, fmt.Errorf("port %d is declared twice: %s %q and %s %q",
				port.Port, first.Source, first.Tag, port.Source, port.Tag)
		}
		seen[port.Port] = port
		relayed = append(relayed, port.Port)
		inbounds = append(inbounds, relayInbound(listenJSON, nextHopHost, port.Port, port.Network))
	}

	if len(inbounds) == 0 {
		return nil, nil, fmt.Errorf("the chain document carries no ports; %s", PortsHint)
	}

	cfg := &xray.Config{
		LogConfig:       json_util.RawMessage(`{"loglevel":"warning"}`),
		InboundConfigs:  inbounds,
		OutboundConfigs: json_util.RawMessage(`[{"protocol":"freedom","tag":"direct"}]`),
	}
	return cfg, relayed, nil
}

// Relay manages the dokodemo-door xray process that forwards traffic to the
// next hop. It is rebuilt and restarted whenever an applied chain revision
// changes the relayed ports or the next hop's address (§3.5): dokodemo-door
// has no soft reload, so the process is replaced.
type Relay struct {
	listen string

	mu          sync.Mutex
	proc        *xray.Process
	ports       []int
	host        string
	restartedAt int64
}

// NewRelay prepares a relay that binds on listen. Nothing runs until Apply.
func NewRelay(listen string) *Relay {
	if listen == "" {
		listen = DefaultRelayListen
	}
	return &Relay{listen: listen}
}

// Apply makes the running relay match ports and nextHopHost, restarting xray.
// A failed start leaves the relay stopped and the error to the caller, which
// keeps the document and retries on the next revision (§3.5).
func (r *Relay) Apply(ports []chain.Port, nextHopHost string) error {
	xrayCfg, relayed, err := BuildRelayConfig(ports, nextHopHost, r.listen)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.proc != nil {
		if err := r.proc.Stop(); err != nil {
			logger.Warning("proxy-front: error stopping the relay before a restart:", err)
		}
		r.proc = nil
	}
	proc := xray.NewTestProcess(xrayCfg, relayConfigPath())
	if err := proc.Start(); err != nil {
		r.ports = nil
		return fmt.Errorf("start relay xray: %w", err)
	}
	r.proc = proc
	r.ports = relayed
	r.host = nextHopHost
	r.restartedAt = time.Now().UnixMilli()
	logger.Infof("proxy-front: relaying ports %v -> %s via dokodemo-door (L4 passthrough)", relayed, nextHopHost)
	return nil
}

// Ports returns the ports currently being relayed.
func (r *Relay) Ports() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.ports...)
}

// Running reports whether the relay xray process is up.
func (r *Relay) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proc != nil && r.proc.IsRunning()
}

// RestartedAt is when the relay last came up, in milliseconds.
func (r *Relay) RestartedAt() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restartedAt
}

// Stop terminates the relay xray process and removes its generated config.
func (r *Relay) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer os.Remove(relayConfigPath())
	if r.proc == nil {
		return nil
	}
	err := r.proc.Stop()
	r.proc = nil
	r.ports = nil
	return err
}
