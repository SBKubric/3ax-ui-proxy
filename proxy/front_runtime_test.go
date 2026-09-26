package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

// frontLog is the order things happened in on the fake machine: the relay
// has to let go of 443/tcp before nginx can take it.
type frontLog struct{ events []string }

func (l *frontLog) add(format string, args ...any) {
	l.events = append(l.events, fmt.Sprintf(format, args...))
}

func (l *frontLog) index(prefix string) int {
	for i, event := range l.events {
		if strings.HasPrefix(event, prefix) {
			return i
		}
	}
	return -1
}

func (l *frontLog) has(prefix string) bool { return l.index(prefix) >= 0 }

type fakeFrontSystem struct {
	log      *frontLog
	noCert   bool
	applyErr error
	current  string // the stream config nginx "runs"
	firewall *nginx.Firewall
}

func (f *fakeFrontSystem) IPCertificate() (string, string, error) {
	if f.noCert {
		return "", "", errors.New("no IP certificate")
	}
	return testIPCert, testIPKey, nil
}
func (f *fakeFrontSystem) WriteStub(html string) error { f.log.add("stub %d", len(html)); return nil }
func (f *fakeFrontSystem) NeedsUpdate(c nginx.Config) (bool, error) {
	stream, err := c.StreamConf()
	return stream != f.current, err
}
func (f *fakeFrontSystem) Apply(c nginx.Config) error {
	f.log.add("nginx.apply")
	if f.applyErr != nil {
		return f.applyErr
	}
	f.current, _ = c.StreamConf()
	return nil
}
func (f *fakeFrontSystem) Remove() error { f.log.add("nginx.remove"); f.current = ""; return nil }
func (f *fakeFrontSystem) ApplyFirewall(fw nginx.Firewall) error {
	f.log.add("firewall.apply tcp=%s udp=%s", portList(fw.TCP), portList(fw.UDP))
	f.firewall = &fw
	return nil
}
func (f *fakeFrontSystem) RemoveFirewall() error {
	f.log.add("firewall.remove")
	f.firewall = nil
	return nil
}

type fakeListeners struct{ log *frontLog }

func (f *fakeListeners) ServeLoopback(addr string) error {
	f.log.add("sub.loopback %s", addr)
	return nil
}
func (f *fakeListeners) StopLoopback()      { f.log.add("sub.stopLoopback") }
func (f *fakeListeners) ClosePublic() error { f.log.add("sub.closePublic"); return nil }
func (f *fakeListeners) OpenPublic() error  { f.log.add("sub.openPublic"); return nil }

type loggingRelay struct {
	recordingRelay
	log     *frontLog
	stopped bool
}

func (r *loggingRelay) Apply(ports []chain.Port, host string) error {
	var parts []string
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%d/%s", port.Port, port.Network))
	}
	r.log.add("relay.apply %s", strings.Join(parts, ","))
	r.stopped = false
	return r.recordingRelay.Apply(ports, host)
}
func (r *loggingRelay) Stop() error { r.log.add("relay.stop"); r.stopped = true; return nil }

type frontRig struct {
	front *Front
	cfg   *Config
	state *State
	sys   *fakeFrontSystem
	log   *frontLog
	relay *loggingRelay
}

func newFrontRig(t *testing.T, mode string, stateDir string) *frontRig {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	log := &frontLog{}
	cfg := &Config{SubPort: DefaultSubPort, StateDir: stateDir, Front: FrontConfig{Mode: mode},
		CertFile: "c.pem", KeyFile: "k.pem"}
	state := NewState()
	sys := &fakeFrontSystem{log: log}
	relay := &loggingRelay{log: log}
	front := NewFront(cfg, state, relay, &fakeListeners{log: log}, sys, "<html>decoy</html>")
	return &frontRig{front: front, cfg: cfg, state: state, sys: sys, log: log, relay: relay}
}

func (r *frontRig) apply(t *testing.T, doc *chain.Document) error {
	t.Helper()
	r.state.SetDocument(doc)
	return r.front.Apply(doc, true)
}

// TestFrontComesUpOnAnEdge: the relay lets go of 443/tcp and the TCP ports the
// front closes, the sub server moves behind nginx, nginx takes 443, and — an
// edge having nobody that polls it — the old sub port closes at once and the
// firewall opens 443, 80 and the relayed UDP ports only.
func TestFrontComesUpOnAnEdge(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	doc := edgeFrontDocument()
	if err := rig.apply(t, doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	layout := NewFrontLayout(doc, DefaultSubPort)

	relay, nginxAt := rig.log.index("relay.apply"), rig.log.index("nginx.apply")
	if relay < 0 || nginxAt < 0 || relay > nginxAt {
		t.Fatalf("the relay must release 443/tcp before nginx takes it: %v", rig.log.events)
	}
	if got := rig.log.events[relay]; got != "relay.apply 443/udp,51820/udp" {
		t.Errorf("relay = %q, want UDP only", got)
	}
	if !rig.log.has("sub.loopback " + layout.SubListen) {
		t.Errorf("the sub server did not move behind the front: %v", rig.log.events)
	}
	if !rig.log.has("stub ") {
		t.Error("no decoy page was written")
	}
	if !rig.cfg.FrontActive() {
		t.Error("the front is up but the box does not say so")
	}
	if !rig.log.has("sub.closePublic") {
		t.Error("an edge kept its old sub port although nobody polls it")
	}
	if rig.sys.firewall == nil || portList(rig.sys.firewall.TCP) != "443,80" || portList(rig.sys.firewall.UDP) != "443,51820" {
		t.Errorf("firewall = %+v, want tcp 443,80 and the relayed UDP ports", rig.sys.firewall)
	}

	// The same document again changes nothing on the machine.
	rig.log.events = nil
	if err := rig.front.Apply(doc, false); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, event := range rig.log.events {
		if strings.HasPrefix(event, "nginx.") || strings.HasPrefix(event, "relay.") || strings.HasPrefix(event, "firewall.") {
			t.Errorf("an unchanged document touched the machine: %v", rig.log.events)
			break
		}
	}
}

// TestFrontOnAnInnerKeepsTheOldPortUntilTheNeighboursMoved: the inner's outer
// neighbours poll its old sub port until a document sends them to 443. Only
// when each has acknowledged a revision newer than the one the front came up
// on does the port close — and the firewall with it.
func TestFrontOnAnInnerKeepsTheOldPortUntilTheNeighboursMoved(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	doc := innerFrontDocument() // revision 42, edges edge-a and edge-b poll it
	if err := rig.apply(t, doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rig.log.has("sub.closePublic") {
		t.Fatal("the old sub port closed before any neighbour moved")
	}
	if got := portList(rig.sys.firewall.TCP); got != "443,80,2096" {
		t.Errorf("firewall TCP = %s, want the old sub port open while the neighbours move", got)
	}

	// edge-a has moved, edge-b is still on revision 42.
	rig.state.RecordOuter(OuterAck{Name: "edge-a", LastRevision: 43, LastSeen: 1}, OuterAck{Name: "edge-b", LastRevision: 42, LastSeen: 1})
	rig.front.NoteAcks()
	if rig.log.has("sub.closePublic") {
		t.Fatal("the old sub port closed while edge-b still polls it")
	}

	rig.state.RecordOuter(OuterAck{Name: "edge-b", LastRevision: 43, LastSeen: 2})
	rig.front.NoteAcks()
	if !rig.log.has("sub.closePublic") {
		t.Fatal("the old sub port stayed open after every neighbour moved")
	}
	if got := portList(rig.sys.firewall.TCP); got != "443,80" {
		t.Errorf("firewall TCP = %s, want the old sub port closed too", got)
	}

	// A restart remembers: the port does not open for the neighbours again.
	again := newFrontRig(t, chain.FrontOnly443, rig.cfg.StateDir)
	if err := again.apply(t, doc); err != nil {
		t.Fatalf("Apply after a restart: %v", err)
	}
	if !again.log.has("sub.closePublic") || portList(again.sys.firewall.TCP) != "443,80" {
		t.Errorf("after a restart the old port came back: %v", again.log.events)
	}
}

// TestFrontWithoutACertificateStaysOff: without the IP certificate the HTTP
// side cannot answer by address, and the sub server would be unreachable
// behind it. The box keeps relaying everything, 443/tcp included, as before.
func TestFrontWithoutACertificateStaysOff(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	rig.sys.noCert = true
	if err := rig.apply(t, edgeFrontDocument()); err == nil {
		t.Error("a front without a certificate reported no problem")
	}
	if rig.cfg.FrontActive() || rig.log.has("nginx.apply") || rig.log.has("firewall.apply") {
		t.Errorf("the front came up without a certificate: %v", rig.log.events)
	}
	if !rig.log.has("relay.apply 443/tcp,udp,8081/tcp,51820/udp") {
		t.Errorf("the relay does not carry every port: %v", rig.log.events)
	}
}

// TestFrontThatNginxRefusesGivesThePortsBack: nginx refusing the config must
// not leave the box with 443/tcp released and nobody on it.
func TestFrontThatNginxRefusesGivesThePortsBack(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	rig.sys.applyErr = errors.New("nginx -t: boom")
	if err := rig.apply(t, edgeFrontDocument()); err == nil {
		t.Error("a refused config reported no problem")
	}
	if rig.cfg.FrontActive() {
		t.Error("the front counts as up although nginx refused it")
	}
	last := -1
	for i, event := range rig.log.events {
		if strings.HasPrefix(event, "relay.apply") {
			last = i
		}
	}
	if last < 0 || rig.log.events[last] != "relay.apply 443/tcp,udp,8081/tcp,51820/udp" {
		t.Errorf("the relay did not get every port back: %v", rig.log.events)
	}
	if rig.log.has("firewall.apply") || rig.log.has("sub.closePublic") {
		t.Errorf("a front that never came up closed ports: %v", rig.log.events)
	}
}

// TestFrontSwitchedOff: the owner turned only443 off in proxy.json. nginx
// lets go of 443, the relay takes every port again, the firewall goes, and the
// old sub port answers once more.
func TestFrontSwitchedOff(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	doc := edgeFrontDocument()
	if err := rig.apply(t, doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	off := newFrontRig(t, chain.FrontOff, rig.cfg.StateDir)
	if err := off.apply(t, doc); err != nil {
		t.Fatalf("Apply with the front off: %v", err)
	}
	for _, want := range []string{"nginx.remove", "firewall.remove", "sub.openPublic", "sub.stopLoopback", "relay.apply 443/tcp,udp,8081/tcp,51820/udp"} {
		if !off.log.has(want) {
			t.Errorf("switching off did not %q: %v", want, off.log.events)
		}
	}
	if off.log.index("nginx.remove") > off.log.index("relay.apply") {
		t.Errorf("nginx must let go of 443 before the relay takes it: %v", off.log.events)
	}
	if _, err := os.Stat(filepath.Join(rig.cfg.StateDir, frontStateFile)); !os.IsNotExist(err) {
		t.Errorf("the front's state outlived it: %v", err)
	}
}

// TestFrontWithTheFirewallSwitchedOff: "firewall": false leaves the ports to
// whatever the box's owner runs instead.
func TestFrontWithTheFirewallSwitchedOff(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOnly443, "")
	off := false
	rig.cfg.Front.Firewall = &off
	if err := rig.apply(t, edgeFrontDocument()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rig.log.has("firewall.apply") {
		t.Errorf("the firewall came up although proxy.json switched it off: %v", rig.log.events)
	}
}

// TestFrontOffIsTheRelayAsBefore: a box without a front relays exactly as it
// always did, and only when the document asks for it.
func TestFrontOffIsTheRelayAsBefore(t *testing.T) {
	rig := newFrontRig(t, chain.FrontOff, "")
	if err := rig.apply(t, edgeFrontDocument()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := strings.Join(rig.log.events, "|"); got != "relay.apply 443/tcp,udp,8081/tcp,51820/udp" {
		t.Errorf("events = %s", got)
	}
	rig.log.events = nil
	if err := rig.front.Apply(edgeFrontDocument(), false); err != nil {
		t.Fatal(err)
	}
	if len(rig.log.events) != 0 {
		t.Errorf("an unchanged document touched the machine: %v", rig.log.events)
	}
}

// TestTheWaveRebuildsTheFront: a revision that changes nothing the relay
// carries can still change the front — here the active edge, whose server
// name an inner passes on. The poller hands every applied revision to it.
func TestTheWaveRebuildsTheFront(t *testing.T) {
	doc := innerFrontDocument()
	poller, _, _, _ := wavePoller(t, serveDocument(&doc, nil))
	poller.cfg.HopSecret = "edge-a-secret"
	rig := newFrontRig(t, chain.FrontOnly443, "")
	rig.front.cfg, rig.front.state = poller.cfg, poller.state
	poller.cfg.Front.Mode, poller.cfg.StateDir = chain.FrontOnly443, t.TempDir()
	poller.front = rig.front

	if err := poller.Apply(doc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !rig.log.has("nginx.apply") {
		t.Fatalf("the first revision did not bring the front up: %v", rig.log.events)
	}

	next := *doc
	next.Revision = 43
	next.ActiveEdge = "edge-b"
	rig.log.events = nil
	if err := poller.Apply(&next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !rig.log.has("nginx.apply") {
		t.Errorf("a new active edge did not reach the front: %v", rig.log.events)
	}
	if rig.log.has("relay.") {
		t.Errorf("the relay restarted for a change it does not carry: %v", rig.log.events)
	}
	if !strings.Contains(rig.sys.current, "cdn.other.example") {
		t.Errorf("the front does not pass edge-b's server name on:\n%s", rig.sys.current)
	}
}
