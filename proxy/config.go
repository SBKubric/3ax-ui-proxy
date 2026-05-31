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
	"strings"
)

// Config is the proxy-front runtime configuration, loaded from a JSON file given
// via `x-ui proxy -c <file>`. The real server address is provided here separately
// from the exported panel xray config (which is only mined for inbound ports).
type Config struct {
	// --- Relay: forwards client traffic to the real server ---

	// UpstreamHost is the real (hidden) server address that relayed traffic is
	// forwarded to (the dokodemo-door destination).
	UpstreamHost string `json:"upstreamHost"`

	// XrayConfigPath points to the real panel's exported xray config.json. Only the
	// inbound ports are read from it, to build the dokodemo-door relay.
	XrayConfigPath string `json:"xrayConfigPath"`

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
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse proxy config %q: %w", path, err)
	}

	cfg.UpstreamHost = strings.TrimSpace(cfg.UpstreamHost)
	cfg.XrayConfigPath = strings.TrimSpace(cfg.XrayConfigPath)
	cfg.UpstreamBase = strings.TrimRight(strings.TrimSpace(cfg.UpstreamBase), "/")
	cfg.Domain = strings.TrimSpace(cfg.Domain)

	if cfg.UpstreamHost == "" {
		return nil, fmt.Errorf("proxy config %q: upstreamHost is required", path)
	}
	if cfg.XrayConfigPath == "" {
		return nil, fmt.Errorf("proxy config %q: xrayConfigPath is required", path)
	}

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
