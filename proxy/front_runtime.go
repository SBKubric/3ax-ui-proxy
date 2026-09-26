package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

// frontStateFile remembers, beside document.json, what the front has done that
// a restart must not undo: the revision it came up on and whether the old sub
// port has already closed.
const frontStateFile = "front.json"

// FrontSystem is the machine the front drives: nginx, the firewall and the
// decoy page. systemFront is the real one; the tests record calls instead.
type FrontSystem interface {
	IPCertificate() (certFile, keyFile string, err error)
	WriteStub(html string) error
	NeedsUpdate(nginx.Config) (bool, error)
	Apply(nginx.Config) error
	Remove() error
	ApplyFirewall(nginx.Firewall) error
	RemoveFirewall() error
}

// frontListeners is the part of the sub server the front moves around.
type frontListeners interface {
	ServeLoopback(addr string) error
	StopLoopback()
	ClosePublic() error
	OpenPublic() error
}

// frontState is front.json.
type frontState struct {
	Mode string `json:"mode"`
	// Since is the revision the box was on when its front came up: a
	// neighbour acknowledging a newer one has moved to 443
	// (OldSubPortNeeded).
	Since         int64 `json:"since"`
	OldPortClosed bool  `json:"oldPortClosed"`
	// SubListen is where the sub server answers behind the front, for
	// `x-ui chain status` once the old port has closed.
	SubListen string `json:"subListen"`
}

// Front is a box's nginx on 443 (ADR 0005, #140), rebuilt from the chain
// document whenever a revision is applied. It owns the relay's port list too,
// because the two share 443: the relay lets go of 443/tcp before nginx takes
// it, and gets it back if nginx cannot.
//
// With the front off in proxy.json it is only the relay, as before.
type Front struct {
	cfg       *Config
	state     *State
	relay     RelayController
	listeners frontListeners
	sys       FrontSystem
	stub      string

	mu            sync.Mutex
	saved         frontState
	relayFiltered bool   // the relay carries the front's port list, not the document's
	publicClosed  bool   // this process has closed the old sub port
	firewall      string // the firewall last applied, "" for none
}

// NewFront prepares the front of a box. stub is the decoy page served when
// proxy.json names none of its own.
func NewFront(cfg *Config, state *State, relay RelayController, listeners frontListeners, sys FrontSystem, stub string) *Front {
	f := &Front{cfg: cfg, state: state, relay: relay, listeners: listeners, sys: sys, stub: stub}
	f.saved = loadFrontState(cfg.StateDir)
	return f
}

// Apply makes the box match doc: the front up or down as proxy.json says, and
// the relay carrying what goes with that. relayChanged is the poller's verdict
// that the ports or the next hop moved, so the relay has to be rebuilt anyway.
func (f *Front) Apply(doc *chain.Document, relayChanged bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if doc == nil {
		return nil
	}
	if !f.cfg.Front.On() {
		return f.offLocked(doc, relayChanged)
	}

	certFile, keyFile, err := f.sys.IPCertificate()
	if err != nil {
		// Without the certificate the HTTP side cannot answer by address,
		// and behind it the sub server would be out of reach altogether.
		// The box stays as it was without a front rather than going dark.
		if offErr := f.offLocked(doc, relayChanged); offErr != nil {
			logger.Warning("proxy-front:", offErr)
		}
		return fmt.Errorf("the front stays off: %w", err)
	}
	layout := NewFrontLayout(doc, f.cfg.SubPort)
	config, warnings, err := BuildFront(doc, layout, certFile, keyFile)
	if err != nil {
		if offErr := f.offLocked(doc, relayChanged); offErr != nil {
			logger.Warning("proxy-front:", offErr)
		}
		return fmt.Errorf("the front stays off: %w", err)
	}

	kept, dropped := RelayPorts(doc.Ports, true)
	wasActive := f.cfg.FrontActive()
	// The relay first: nginx cannot bind 443 while dokodemo holds it.
	if relayChanged || !f.relayFiltered {
		if len(dropped) > 0 {
			logger.Warningf("proxy-front: the front leaves TCP on 443 only — not relaying TCP ports %v", dropped)
		}
		if err := f.applyRelay(kept, doc.NextHop.Host); err != nil {
			return f.giveBackLocked(doc, fmt.Errorf("relay behind the front: %w", err))
		}
		f.relayFiltered = true
	}
	if err := f.listeners.ServeLoopback(layout.SubListen); err != nil {
		return f.giveBackLocked(doc, err)
	}

	needs, err := f.sys.NeedsUpdate(config)
	if err != nil || needs || !wasActive {
		for _, warning := range warnings {
			logger.Warning("proxy-front:", warning)
		}
		if err := f.sys.WriteStub(f.stubHTML()); err != nil {
			logger.Warning("proxy-front: the decoy page:", err)
		}
		if err := f.sys.Apply(config); err != nil {
			return f.giveBackLocked(doc, fmt.Errorf("nginx: %w", err))
		}
		logger.Infof("proxy-front: the front is up on %d for revision %d (%s)", frontPort, doc.Revision, doc.Self.Role)
	}
	f.cfg.SetFrontActive(true)

	if f.saved.Mode != chain.FrontOnly443 {
		f.saved = frontState{Mode: chain.FrontOnly443, Since: doc.Revision}
	}
	f.saved.SubListen = layout.SubListen
	f.saveLocked()
	f.portsLocked(doc, kept)
	return nil
}

// NoteAcks is called whenever an outer neighbour has polled: its
// acknowledgement may be the last one the old sub port was waiting for.
func (f *Front) NoteAcks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc := f.state.Document()
	if doc == nil || !f.cfg.FrontActive() {
		return
	}
	kept, _ := RelayPorts(doc.Ports, true)
	f.portsLocked(doc, kept)
}

// portsLocked closes the old sub port once nobody needs it and keeps the
// firewall in step with what is open.
func (f *Front) portsLocked(doc *chain.Document, relayed []chain.Port) {
	if !f.saved.OldPortClosed && !OldSubPortNeeded(doc, f.saved.Since, f.state.OuterAcks()) {
		f.saved.OldPortClosed = true
		f.saveLocked()
	}
	if f.saved.OldPortClosed && !f.publicClosed {
		if err := f.listeners.ClosePublic(); err != nil {
			logger.Warning("proxy-front: closing the old sub port:", err)
		}
		f.publicClosed = true
	}

	if !f.cfg.Front.FirewallOn() {
		f.removeFirewallLocked()
		return
	}
	oldPort := 0
	if !f.saved.OldPortClosed {
		oldPort = f.cfg.SubPort
	}
	fw := FrontFirewall(relayed, oldPort)
	key := fmt.Sprintf("tcp=%v udp=%v", fw.TCP, fw.UDP)
	if key == f.firewall {
		return
	}
	if err := f.sys.ApplyFirewall(fw); err != nil {
		logger.Warning("proxy-front: the firewall:", err)
		return
	}
	f.firewall = key
	logger.Infof("proxy-front: firewall: only %s stay open (plus SSH)", key)
}

func (f *Front) removeFirewallLocked() {
	if f.firewall == "" {
		// Nothing of ours was applied by this process; a chain left over
		// from before a restart goes too.
		f.firewall = "none"
		_ = f.sys.RemoveFirewall()
		return
	}
	if f.firewall != "none" {
		if err := f.sys.RemoveFirewall(); err != nil {
			logger.Warning("proxy-front: removing the firewall:", err)
		}
		f.firewall = "none"
	}
}

// giveBackLocked undoes a front that could not come up: the relay gets every
// port of the document back, 443/tcp first among them, so a failed nginx
// costs the box nothing it had before.
func (f *Front) giveBackLocked(doc *chain.Document, cause error) error {
	f.cfg.SetFrontActive(false)
	f.listeners.StopLoopback()
	if err := f.applyRelay(doc.Ports, doc.NextHop.Host); err != nil {
		logger.Warning("proxy-front: giving the relay its ports back:", err)
	}
	f.relayFiltered = false
	return fmt.Errorf("the front did not come up, relaying every port as before: %w", cause)
}

// offLocked is the box without a front. A front that was up — in this process
// or before a restart — is taken down in the order that frees 443 for the
// relay: nginx first, then the relay with every port of the document.
func (f *Front) offLocked(doc *chain.Document, relayChanged bool) error {
	wasOn := f.cfg.FrontActive() || f.saved.Mode == chain.FrontOnly443 || f.relayFiltered
	if !wasOn {
		if !relayChanged {
			return nil
		}
		return f.applyRelay(doc.Ports, doc.NextHop.Host)
	}

	f.cfg.SetFrontActive(false)
	var errs []error
	if err := f.sys.Remove(); err != nil {
		errs = append(errs, fmt.Errorf("nginx: %w", err))
	}
	if err := f.sys.RemoveFirewall(); err != nil {
		errs = append(errs, fmt.Errorf("firewall: %w", err))
	}
	f.firewall = "none"
	if err := f.listeners.OpenPublic(); err != nil {
		errs = append(errs, err)
	}
	f.publicClosed = false
	f.listeners.StopLoopback()
	if err := f.applyRelay(doc.Ports, doc.NextHop.Host); err != nil {
		errs = append(errs, err)
	}
	f.relayFiltered = false
	f.saved = frontState{}
	if err := os.Remove(filepath.Join(f.cfg.StateDir, frontStateFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	logger.Info("proxy-front: the front is off; the relay carries every port and the sub port answers again")
	return errors.Join(errs...)
}

// applyRelay rebuilds the relay, or stops it when the front left it nothing
// to carry — a chain whose only port is 443/tcp.
func (f *Front) applyRelay(ports []chain.Port, nextHopHost string) error {
	if len(ports) == 0 {
		return f.relay.Stop()
	}
	return f.relay.Apply(ports, nextHopHost)
}

// stubHTML is the decoy: the owner's file when proxy.json names one, the
// built-in page otherwise.
func (f *Front) stubHTML() string {
	if f.cfg.Front.Stub != "" {
		if body, err := os.ReadFile(f.cfg.Front.Stub); err == nil && len(strings.TrimSpace(string(body))) > 0 {
			return string(body)
		} else if err != nil {
			logger.Warning("proxy-front: the decoy page in proxy.json cannot be read, using the built-in one:", err)
		}
	}
	return f.stub
}

func (f *Front) saveLocked() {
	data, err := json.MarshalIndent(f.saved, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(filepath.Join(f.cfg.StateDir, frontStateFile), append(data, '\n'), 0o600); err != nil {
		logger.Warning("proxy-front: remembering the front:", err)
	}
}

// loadFrontState reads front.json; a missing or unreadable file is a box whose
// front has never been up.
func loadFrontState(stateDir string) frontState {
	var saved frontState
	data, err := os.ReadFile(filepath.Join(stateDir, frontStateFile))
	if err != nil {
		return saved
	}
	_ = json.Unmarshal(data, &saved)
	return saved
}

// ipCertDir is where install.sh keeps the box's Let's Encrypt IP certificate
// (#142) — the one the HTTP side answers requests by address with.
var ipCertDir = "/root/cert/ip"

// systemFront is the real machine: nginx and iptables through package nginx.
type systemFront struct{}

func (systemFront) IPCertificate() (string, string, error) {
	cert, key := filepath.Join(ipCertDir, "fullchain.pem"), filepath.Join(ipCertDir, "privkey.pem")
	for _, path := range []string{cert, key} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			return "", "", fmt.Errorf("no IP certificate at %s — issue one first (docs/runbooks/proxy-front.md)", path)
		}
	}
	return cert, key, nil
}
func (systemFront) WriteStub(html string) error              { return nginx.WriteStub(html) }
func (systemFront) NeedsUpdate(c nginx.Config) (bool, error) { return nginx.NeedsUpdate(c) }
func (systemFront) Apply(c nginx.Config) error               { return nginx.Apply(c) }
func (systemFront) Remove() error                            { return nginx.Remove() }
func (systemFront) ApplyFirewall(fw nginx.Firewall) error    { return nginx.ApplyFirewall(fw) }
func (systemFront) RemoveFirewall() error                    { return nginx.RemoveFirewall() }
