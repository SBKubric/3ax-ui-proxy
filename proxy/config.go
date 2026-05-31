// Package proxy implements the "proxy front" run mode: a sacrificial relay that
// L4-forwards client traffic to the real (hidden) panel server via xray
// dokodemo-door, and (separately) serves subscriptions fetched from the real
// panel. When the front gets blocked it is thrown away and replaced, while the
// real server — whose address never appears in client configs — keeps running.
package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the proxy-front runtime configuration, loaded from a JSON file given
// via `x-ui proxy -c <file>`. The real server address is provided here separately
// from the exported panel xray config (which is only mined for inbound ports).
type Config struct {
	// UpstreamHost is the real (hidden) server address that relayed traffic is
	// forwarded to (the dokodemo-door destination).
	UpstreamHost string `json:"upstreamHost"`

	// XrayConfigPath points to the real panel's exported xray config.json. Only the
	// inbound ports are read from it, to build the dokodemo-door relay.
	XrayConfigPath string `json:"xrayConfigPath"`

	// UpstreamSubURL is the base subscription URL on the real panel that the proxy
	// fetches from, e.g. "https://1.2.3.4:2096/sub-xxxx/". Used by the sub server.
	UpstreamSubURL string `json:"upstreamSubURL"`

	// Proxy's own subscription endpoint (the page clients open).
	Domain    string `json:"domain"`
	SubListen string `json:"subListen"`
	SubPort   int    `json:"subPort"`
	SubPath   string `json:"subPath"`
	CertFile  string `json:"cert"`
	KeyFile   string `json:"key"`
}

// LoadConfig reads and validates the proxy config from a JSON file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read proxy config %q: %w", path, err)
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse proxy config %q: %w", path, err)
	}

	cfg.UpstreamHost = strings.TrimSpace(cfg.UpstreamHost)
	cfg.XrayConfigPath = strings.TrimSpace(cfg.XrayConfigPath)
	if cfg.UpstreamHost == "" {
		return nil, fmt.Errorf("proxy config: %q: upstreamHost is required", path)
	}
	if cfg.XrayConfigPath == "" {
		return nil, fmt.Errorf("proxy config: %q: xrayConfigPath is required", path)
	}
	return cfg, nil
}
