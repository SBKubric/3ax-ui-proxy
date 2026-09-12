// Package proxy implements the "proxy front" run mode: a sacrificial relay that
// L4-forwards client traffic to the real (hidden) panel server via xray
// dokodemo-door, and serves subscriptions fetched from the real panel. When the
// front gets blocked it is thrown away and replaced, while the real server —
// whose address never appears in client configs — keeps running.
package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the proxy-front runtime configuration, loaded from a JSON file given
// via `x-ui proxy -c <file>`. The real server address is provided here separately
// from the relay manifest (which only lists the inbound ports to relay).
type Config struct {
	// --- Relay: forwards client traffic to the real server ---

	// UpstreamHost is the real (hidden) server address that relayed traffic is
	// forwarded to (the dokodemo-door destination).
	UpstreamHost string `json:"upstreamHost"`

	// RelayManifestPath points to the relay manifest exported from the real
	// panel (`x-ui relay-manifest`): the sanitised list of inbounds the relay
	// opens ports for. A raw panel config.json is refused. When the file does
	// not exist yet, `x-ui proxy` starts in bootstrap mode and serves a setup
	// page to paste it in (see setup.go); the file is written here.
	RelayManifestPath string `json:"relayManifestPath"`

	// RelayListen is the address the dokodemo-door relay binds on. Defaults to
	// "::" (dual-stack — accepts both IPv4 and IPv6); set "0.0.0.0" on hosts with
	// IPv6 disabled.
	RelayListen string `json:"relayListen"`

	// ExtraPorts lists the real server's public ports that are not xray inbounds
	// and therefore never appear in RelayManifestPath — AmneziaWG / WireGuard
	// listeners, the MTProto sidecar — but must be relayed all the same. Each
	// entry is "<port>/<tcp|udp|tcp+udp>"; the protocol suffix is mandatory. A
	// port that is also an xray inbound in the panel config is an error.
	ExtraPorts []string `json:"extraPorts"`

	extraPorts []ExtraPort
	path       string // where this config was loaded from (for SetupURLPath)

	// --- Subscription server: the proxy's own /sub + /json endpoints, proxied
	// from the real panel. Enabled when UpstreamBase is set. ---

	// UpstreamBase is the real panel's subscription server base, reachable from the
	// proxy (often by IP), e.g. "https://1.2.3.4:2096". The proxy fetches
	// UpstreamBase+SubPath+id and UpstreamBase+JsonPath+id from it.
	UpstreamBase string `json:"upstreamBase"`

	// Domain is the proxy's public host advertised in the sub URLs it hands out
	// (defaults to the request Host when empty).
	Domain    string `json:"domain"`
	SubListen string `json:"subListen"` // "" = all interfaces
	SubPort   int    `json:"subPort"`   // default 2096
	SubPath   string `json:"subPath"`   // default "/sub/"
	JsonPath  string `json:"jsonPath"`  // default "/json/"
	CertFile  string `json:"cert"`
	KeyFile   string `json:"key"`
}

// ExtraPort is one relayed port that the real server serves outside xray
// (see Config.ExtraPorts). Network is the dokodemo-door network selector:
// "tcp", "udp" or "tcp,udp".
type ExtraPort struct {
	Port    int
	Network string
}

// ParseExtraPort parses a "<port>/<tcp|udp|tcp+udp>" entry.
func ParseExtraPort(s string) (ExtraPort, error) {
	s = strings.TrimSpace(s)
	portStr, proto, ok := strings.Cut(s, "/")
	if !ok {
		return ExtraPort{}, fmt.Errorf("extra port %q: protocol suffix required (e.g. \"51820/udp\")", s)
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port < 1 || port > 65535 {
		return ExtraPort{}, fmt.Errorf("extra port %q: port must be 1-65535", s)
	}
	var network string
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "tcp":
		network = "tcp"
	case "udp":
		network = "udp"
	case "tcp+udp", "udp+tcp":
		network = "tcp,udp"
	default:
		return ExtraPort{}, fmt.Errorf("extra port %q: protocol must be tcp, udp or tcp+udp", s)
	}
	return ExtraPort{Port: port, Network: network}, nil
}

// parseExtraPorts parses every entry and rejects duplicate ports.
func parseExtraPorts(entries []string) ([]ExtraPort, error) {
	seen := make(map[int]bool, len(entries))
	out := make([]ExtraPort, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e) == "" {
			continue
		}
		ep, err := ParseExtraPort(e)
		if err != nil {
			return nil, err
		}
		if seen[ep.Port] {
			return nil, fmt.Errorf("extra port %d listed twice", ep.Port)
		}
		seen[ep.Port] = true
		out = append(out, ep)
	}
	return out, nil
}

// Path is the file this config was loaded from ("" when built in code).
func (c *Config) Path() string { return c.path }

// ExtraRelayPorts returns the parsed ExtraPorts entries.
func (c *Config) ExtraRelayPorts() []ExtraPort { return c.extraPorts }

// SubEnabled reports whether the subscription server should run, i.e. an upstream
// base to fetch subscriptions from has been configured.
func (c *Config) SubEnabled() bool { return c.UpstreamBase != "" }

// TLS reports whether the proxy serves its subscription endpoint over HTTPS.
func (c *Config) TLS() bool { return c.CertFile != "" && c.KeyFile != "" }

// LoadConfig reads and validates the proxy config from a JSON file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read proxy config %q: %w", path, err)
	}
	cfg := &Config{path: path}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse proxy config %q: %w", path, err)
	}

	cfg.UpstreamHost = strings.TrimSpace(cfg.UpstreamHost)
	cfg.RelayManifestPath = strings.TrimSpace(cfg.RelayManifestPath)
	cfg.UpstreamBase = strings.TrimRight(strings.TrimSpace(cfg.UpstreamBase), "/")
	cfg.Domain = strings.TrimSpace(cfg.Domain)

	if cfg.UpstreamHost == "" {
		return nil, fmt.Errorf("proxy config %q: upstreamHost is required", path)
	}
	if cfg.RelayManifestPath == "" {
		return nil, fmt.Errorf("proxy config %q: relayManifestPath is required (the relay manifest exported from the panel; the former xrayConfigPath is gone — a raw panel config is no longer accepted)", path)
	}

	cfg.RelayListen = strings.TrimSpace(cfg.RelayListen)
	if cfg.RelayListen == "" {
		cfg.RelayListen = "::"
	}

	extra, err := parseExtraPorts(cfg.ExtraPorts)
	if err != nil {
		return nil, fmt.Errorf("proxy config %q: %w", path, err)
	}
	cfg.extraPorts = extra

	if cfg.SubEnabled() {
		cfg.SubPath = normalizePath(cfg.SubPath, "/sub/")
		cfg.JsonPath = normalizePath(cfg.JsonPath, "/json/")
		if cfg.SubPort == 0 {
			cfg.SubPort = 2096
		}
	}
	return cfg, nil
}

// normalizePath ensures p is bracketed by single slashes, falling back to def
// when empty.
func normalizePath(p, def string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		p = def
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}
