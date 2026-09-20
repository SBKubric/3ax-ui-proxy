// Package proxy implements the "proxy front" run mode — one hop of the proxy
// chain (docs/spec/proxy-chain.md §5). A hop L4-forwards client traffic to its
// next hop via xray dokodemo-door, serves subscriptions fetched from it, and
// learns what to relay from the chain document it polls from that same next
// hop. When a hop gets blocked it is thrown away and replaced, while the real
// server — whose address never appears in client configs, and which a hop only
// knows through its next hop — keeps running.
package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ConfigVersion is the proxy.json format this build speaks (§5.1). It is the
// version of the box's own file — not of the chain document, and not the
// registry revision. A file claiming a higher one is refused; a file claiming
// none is a v1 file (§5.3).
const ConfigVersion = 2

// Defaults of the keys a proxy.json may leave out (§5.1).
const (
	DefaultSubPort      = 2096
	DefaultSubScheme    = "https"
	DefaultRelayListen  = "::"
	DefaultStateDir     = "/etc/x-ui/chain"
	DefaultPollSeconds  = 30
	DefaultStaleMinutes = 60
)

// NextHop is the one address a hop knows towards the real server: where the
// relay forwards to, where subscriptions come from, and where the chain
// document is polled. For the innermost hop it is the panel itself.
type NextHop struct {
	Host      string `json:"host"`
	SubPort   int    `json:"subPort"`
	SubScheme string `json:"subScheme"`
}

// Config is the proxy-front runtime configuration, loaded from a JSON file
// given via `x-ui proxy -c <file>` (§5.1). Everything the hop relays — ports,
// subscription paths, its own name — arrives in the chain document instead,
// so this file only answers "who is my next hop and what do I prove myself
// with".
type Config struct {
	Version int     `json:"version"`
	NextHop NextHop `json:"nextHop"`

	// HopSecret is the hop secret the panel issued at join: the bearer of
	// GET /chain/v1/document at the next hop. Empty means this box has not
	// joined yet — bootstrap/join mode (§5.4), not an error.
	HopSecret string `json:"hopSecret"`

	SubListen   string `json:"subListen"`
	SubPort     int    `json:"subPort"`
	RelayListen string `json:"relayListen"`
	Domain      string `json:"domain"`
	CertFile    string `json:"cert"`
	KeyFile     string `json:"key"`

	// StateDir holds the last accepted chain document, the one thing that
	// lets a rebooted box relay before the first poll comes back.
	StateDir string `json:"stateDir"`

	// PollSeconds overrides the wave interval for debugging a single box;
	// 0 means "as the chain says" (chainPollSeconds, default 30).
	PollSeconds int `json:"pollSeconds"`

	// StaleMinutes is how long the next hop may stay unreachable before the
	// hop calls itself stale. It never stops relaying (§3.6).
	StaleMinutes int `json:"staleMinutes"`

	path           string   // where this config was loaded from
	legacyWarnings []string // one line per v1 key found (§5.3)
	legacyNextHop  string   // v1 upstreamHost, kept only as a join-page prefill
}

// legacyKeys are the v1 keys and what replaced them. A v1 proxy.json reaches a
// live box through update.sh long before its owner does (§5.3), so each one is
// a WARN naming its replacement — never a refusal.
var legacyKeys = []struct{ key, replacement string }{
	{"upstreamHost", `the chain takes it from "nextHop.host" (join this box: x-ui chain join-url)`},
	{"relayManifestPath", "relayed ports now arrive in the chain document"},
	{"extraPorts", `extra ports now live in the panel registry setting "chainExtraPorts" and arrive in the chain document`},
	{"upstreamBase", `the subscription upstream is built from "nextHop"`},
	{"subPath", `subscription paths arrive in the chain document ("nextHop.subPath")`},
	{"jsonPath", `subscription paths arrive in the chain document ("nextHop.jsonPath")`},
}

// Path is the file this config was loaded from ("" when built in code).
func (c *Config) Path() string { return c.path }

// Bootstrap reports whether this box still has to join the chain: no hop
// secret means no document, no relay, and a join page on the sub port (§5.4).
func (c *Config) Bootstrap() bool { return strings.TrimSpace(c.HopSecret) == "" }

// TLS reports whether this hop serves its sub port (subscriptions, the join
// page and /chain/v1/*) over HTTPS.
func (c *Config) TLS() bool { return c.CertFile != "" && c.KeyFile != "" }

// Scheme is how the outside reaches this hop's sub port.
func (c *Config) Scheme() string {
	if c.TLS() {
		return "https"
	}
	return "http"
}

// NextHopBase is the next hop's sub server: subscriptions and /chain/v1/*.
func (c *Config) NextHopBase() string {
	return c.NextHop.SubScheme + "://" + net.JoinHostPort(c.NextHop.Host, strconv.Itoa(c.NextHop.SubPort))
}

// NextHopHint is the address to prefill the join page's "next hop" field with:
// the configured one, else the v1 upstreamHost of a legacy file — the one
// value of a v1 config worth anything to its owner.
func (c *Config) NextHopHint() string {
	if c.NextHop.Host != "" {
		return c.NextHop.Host
	}
	return c.legacyNextHop
}

// LegacyWarnings is one line per v1 key found in the file, for Run to log.
func (c *Config) LegacyWarnings() []string { return c.legacyWarnings }

// DocumentPath is where the last accepted chain document is cached (§5.5).
func (c *Config) DocumentPath() string { return filepath.Join(c.StateDir, "document.json") }

// Poll is the wave interval this box uses when the chain does not say
// otherwise.
func (c *Config) Poll() int {
	if c.PollSeconds > 0 {
		return c.PollSeconds
	}
	return DefaultPollSeconds
}

// LoadConfig reads the proxy config, applies the defaults of §5.1 and decides
// which of the three modes of §5.3 the file is in. It fails only on a version
// newer than this build: a box that cannot parse its config is a box with no
// service, and the running relay is worth more than the diagnosis.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read proxy config %q: %w", path, err)
	}
	cfg := &Config{path: path}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse proxy config %q: %w", path, err)
	}
	if cfg.Version > ConfigVersion {
		return nil, fmt.Errorf("proxy config %q: version %d is newer than this build understands (%d)", path, cfg.Version, ConfigVersion)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse proxy config %q: %w", path, err)
	}
	for _, legacy := range legacyKeys {
		if _, found := raw[legacy.key]; !found {
			continue
		}
		cfg.legacyWarnings = append(cfg.legacyWarnings,
			fmt.Sprintf("proxy config %s: %q is a v1 key, ignored — %s", path, legacy.key, legacy.replacement))
		if legacy.key == "upstreamHost" {
			var host string
			_ = json.Unmarshal(raw[legacy.key], &host)
			cfg.legacyNextHop = strings.TrimSpace(host)
		}
	}
	// A file with no version marker is v1 whatever else it carries: its
	// values say nothing about the chain, which hands out the secret and the
	// ports only through a join.
	if cfg.Version < ConfigVersion {
		cfg.NextHop = NextHop{}
		cfg.HopSecret = ""
	}

	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults fills in every key §5.1 lets a file leave out.
func (c *Config) applyDefaults() {
	c.NextHop.Host = strings.TrimSpace(c.NextHop.Host)
	if c.NextHop.SubPort == 0 {
		c.NextHop.SubPort = DefaultSubPort
	}
	c.NextHop.SubScheme = strings.ToLower(strings.TrimSpace(c.NextHop.SubScheme))
	if c.NextHop.SubScheme != "http" {
		c.NextHop.SubScheme = DefaultSubScheme
	}
	c.HopSecret = strings.TrimSpace(c.HopSecret)
	c.Domain = strings.TrimSpace(c.Domain)
	c.SubListen = strings.TrimSpace(c.SubListen)
	if c.SubPort == 0 {
		c.SubPort = DefaultSubPort
	}
	c.RelayListen = strings.TrimSpace(c.RelayListen)
	if c.RelayListen == "" {
		c.RelayListen = DefaultRelayListen
	}
	c.StateDir = strings.TrimSpace(c.StateDir)
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if c.StaleMinutes <= 0 {
		c.StaleMinutes = DefaultStaleMinutes
	}
}

// Save writes the config back — how a box records the hopSecret and the
// nextHop it just joined with. The write is atomic and 0600: a half-written
// proxy.json would cost the box its place in the chain, and the hop secret is
// the one credential it has.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("proxy config: nowhere to save to (config was not loaded from a file)")
	}
	c.Version = ConfigVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode proxy config: %w", err)
	}
	if err := writeFileAtomic(c.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write proxy config %q: %w", c.path, err)
	}
	return nil
}

// writeFileAtomic writes data to a sibling temporary file, fsyncs it and
// renames it over path (§3.6). Both the config and the cached chain document
// are read by the box at boot, when nothing can fix a truncated file: a rename
// either happened or did not.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
