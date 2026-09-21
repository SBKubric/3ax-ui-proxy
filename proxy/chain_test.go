package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// recordingRelay stands in for the xray relay: the poller never starts a
// process in a test, it only says what the relay should carry.
type recordingRelay struct {
	applies  int
	ports    []chain.Port
	nextHop  string
	failWith error
}

func (r *recordingRelay) Apply(ports []chain.Port, nextHopHost string) error {
	r.applies++
	if r.failWith != nil {
		return r.failWith
	}
	r.ports = ports
	r.nextHop = nextHopHost
	return nil
}
func (r *recordingRelay) Running() bool      { return r.failWith == nil && r.applies > 0 }
func (r *recordingRelay) Ports() []int       { return portNumbers(r.ports) }
func (r *recordingRelay) RestartedAt() int64 { return 0 }

func portNumbers(ports []chain.Port) []int {
	out := make([]int, 0, len(ports))
	for _, port := range ports {
		out = append(out, port.Port)
	}
	return out
}

// testDocument is the document a next hop hands this hop.
func testDocument(revision int64, ports ...chain.Port) *chain.Document {
	if len(ports) == 0 {
		ports = []chain.Port{{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray}}
	}
	return &chain.Document{
		Version:     chain.DocumentVersion,
		Revision:    revision,
		GeneratedAt: 1758380000000,
		Self:        chain.Self{Name: "edge-a", Role: chain.RoleEdge, Host: "a.example.net"},
		NextHop: chain.NextHop{
			Host: "10.0.0.7", SubPort: 2096, SubScheme: "https",
			SubPath: "/sub/", JsonPath: "/json/",
		},
		ActiveEdge: "edge-a",
		Hops: []chain.Hop{
			{Name: "edge-a", Role: chain.RoleEdge, Host: "a.example.net", SubPort: 2096, SecretHash: chain.HashSecret("edge-a-secret"), State: chain.StateJoined},
		},
		Ports: ports,
	}
}

// wavePoller wires a poller to a fake next hop and a scratch state dir.
func wavePoller(t *testing.T, handler http.HandlerFunc) (*Poller, *State, *recordingRelay, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	host, port := hostPort(t, server.URL)
	cfg := &Config{
		NextHop:      NextHop{Host: host, SubPort: port, SubScheme: "http"},
		HopSecret:    "edge-a-secret",
		SubPort:      DefaultSubPort,
		StaleMinutes: DefaultStaleMinutes,
	}
	state := NewState()
	relay := &recordingRelay{}
	poller := NewPoller(cfg, state, NewDocumentStore(filepath.Join(t.TempDir(), "document.json")), relay)
	poller.jitter = func(d time.Duration) time.Duration { return d } // deterministic
	return poller, state, relay, server
}

// serveDocument answers like the panel's /chain/v1/document (§3.3).
func serveDocument(doc **chain.Document, seen *http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		if r.Header.Get("Authorization") != "Bearer edge-a-secret" {
			http.NotFound(w, r)
			return
		}
		current := *doc
		if r.Header.Get("If-None-Match") == etag(current.Revision) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag(current.Revision))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(current)
	}
}

// TestPollAppliesTheFirstDocument: a hop that receives a document caches it
// and starts relaying exactly the ports it carries.
func TestPollAppliesTheFirstDocument(t *testing.T) {
	doc := testDocument(42)
	poller, state, relay, _ := wavePoller(t, serveDocument(&doc, nil))

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := state.Revision(); got != 42 {
		t.Errorf("revision = %d, want 42", got)
	}
	if relay.applies != 1 || relay.nextHop != "10.0.0.7" || len(relay.ports) != 1 {
		t.Errorf("relay applied %d times with %v -> %q", relay.applies, relay.ports, relay.nextHop)
	}
	cached, err := poller.store.Load()
	if err != nil || cached == nil || cached.Revision != 42 {
		t.Fatalf("document was not cached: %v %v", cached, err)
	}
	if st, err := os.Stat(poller.store.Path()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("cached document mode = %v (%v), want 600", st.Mode().Perm(), err)
	}
}

// TestPollSendsSeenAndOuterAcks: the wave carries the freshness of everything
// outward from this hop inward, in headers rather than a GET body (§3.3).
func TestPollSendsSeenAndOuterAcks(t *testing.T) {
	doc := testDocument(42)
	var seen http.Header
	poller, state, _, _ := wavePoller(t, serveDocument(&doc, &seen))

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	state.RecordOuter(OuterAck{Name: "edge-b", LastRevision: 41, LastSeen: 1758379990000})

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := seen.Get("X-Chain-Seen"); got != "42" {
		t.Errorf("X-Chain-Seen = %q, want the applied revision", got)
	}
	if got := seen.Get("If-None-Match"); got != `"42"` {
		t.Errorf("If-None-Match = %q", got)
	}
	acks := DecodeOuterAcks(seen.Get("X-Chain-Outer"))
	if len(acks) != 1 || acks[0].Name != "edge-b" || acks[0].LastRevision != 41 {
		t.Errorf("X-Chain-Outer carried %+v, want edge-b's ack", acks)
	}
}

// TestPollOn304ChangesNothing: an unchanged revision must not restart a relay
// — a restart drops every established connection (§3.5).
func TestPollOn304ChangesNothing(t *testing.T) {
	doc := testDocument(42)
	poller, _, relay, _ := wavePoller(t, serveDocument(&doc, nil))

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if relay.applies != 1 {
		t.Errorf("relay applied %d times, want 1 — the second poll was a 304", relay.applies)
	}
}

// TestPollOn404KeepsRelaying: a revoked or not-yet-known hop gets a bare 404
// and goes on relaying the last document it has. There is no self-shutdown
// (§3.6, §3.7).
func TestPollOn404KeepsRelaying(t *testing.T) {
	doc := testDocument(42)
	poller, state, relay, _ := wavePoller(t, serveDocument(&doc, nil))
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	poller.cfg.HopSecret = "revoked"
	err := poller.PollOnce(context.Background())
	if err == nil {
		t.Fatal("a 404 must be reported")
	}
	if state.Revision() != 42 {
		t.Errorf("revision = %d, want the last known 42", state.Revision())
	}
	if relay.applies != 1 {
		t.Errorf("the relay was touched on a 404 (%d applies)", relay.applies)
	}
	if got := poller.Interval(); got <= 0 {
		t.Errorf("Interval() after a failure = %v", got)
	}
}

// TestApplyDiffs pins §3.5: ports and the next hop's address restart the
// relay, an activeEdge switch does not touch the box at all.
func TestApplyDiffs(t *testing.T) {
	doc := testDocument(42)
	poller, _, relay, _ := wavePoller(t, serveDocument(&doc, nil))
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// Only the active edge changed: the box does nothing.
	switched := testDocument(43)
	switched.ActiveEdge = "edge-b"
	if err := poller.Apply(switched); err != nil {
		t.Fatalf("Apply(activeEdge): %v", err)
	}
	if relay.applies != 1 {
		t.Errorf("an activeEdge switch restarted the relay (%d applies)", relay.applies)
	}

	// A port arrives: rebuild and restart.
	morePorts := testDocument(44,
		chain.Port{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		chain.Port{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg})
	if err := poller.Apply(morePorts); err != nil {
		t.Fatalf("Apply(ports): %v", err)
	}
	if relay.applies != 2 || len(relay.ports) != 2 {
		t.Errorf("a new port did not reach the relay: %d applies, %v", relay.applies, relay.ports)
	}

	// The chain is re-plumbed: same ports, new next hop.
	moved := testDocument(45,
		chain.Port{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		chain.Port{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg})
	moved.NextHop.Host = "10.0.0.9"
	if err := poller.Apply(moved); err != nil {
		t.Fatalf("Apply(nextHop): %v", err)
	}
	if relay.applies != 3 || relay.nextHop != "10.0.0.9" {
		t.Errorf("a new next hop did not reach the relay: %d applies, %q", relay.applies, relay.nextHop)
	}
}

// TestApplyKeepsTheDocumentWhenTheRelayFails: the document is written before
// it is applied and kept even if applying fails, or the hop would ask for it,
// fail, drop it and loop forever (§3.5).
func TestApplyKeepsTheDocumentWhenTheRelayFails(t *testing.T) {
	doc := testDocument(42)
	poller, state, relay, _ := wavePoller(t, serveDocument(&doc, nil))
	relay.failWith = errRelayRefused

	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("a failing relay must be reported")
	}
	if state.Revision() != 42 {
		t.Errorf("the document was dropped after a failed apply: revision %d", state.Revision())
	}
	cached, err := poller.store.Load()
	if err != nil || cached == nil || cached.Revision != 42 {
		t.Fatalf("the document was not cached before applying: %v %v", cached, err)
	}
}

var errRelayRefused = &relayError{"relay refused to start"}

type relayError struct{ msg string }

func (e *relayError) Error() string { return e.msg }

// TestStaleAfterTheQuietWindow: past staleMinutes without a 200 or a 304 the
// hop says so — and keeps relaying, with no upper limit on staleness (§3.6).
func TestStaleAfterTheQuietWindow(t *testing.T) {
	doc := testDocument(42)
	reachable := true
	poller, state, _, _ := wavePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if !reachable {
			http.Error(w, "gone", http.StatusBadGateway)
			return
		}
		serveDocument(&doc, nil)(w, r)
	})

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	poller.now = func() time.Time { return now }
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	reachable = false
	now = now.Add(30 * time.Minute)
	_ = poller.PollOnce(context.Background())
	if state.Status(nil, poller.cfg).Stale {
		t.Error("stale after 30 minutes, want stale only past staleMinutes (60)")
	}

	now = now.Add(31 * time.Minute)
	_ = poller.PollOnce(context.Background())
	status := state.Status(nil, poller.cfg)
	if !status.Stale {
		t.Error("not stale after 61 quiet minutes")
	}
	if status.Revision != 42 {
		t.Errorf("a stale hop stopped carrying its revision: %d", status.Revision)
	}
	if status.NextHop.Reachable {
		t.Error("a stale hop reports its next hop as reachable")
	}

	// The next success clears it; nothing ever stops the relay.
	reachable = true
	now = now.Add(time.Minute)
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	if state.Status(nil, poller.cfg).Stale {
		t.Error("still stale after a successful poll")
	}
}

// TestIntervalBacksOffAndJitters: failures slow the wave down to at most five
// minutes, and every interval is spread ±20 % so a chain does not poll in a
// synchronised wave (§3.3, §3.6).
func TestIntervalBacksOffAndJitters(t *testing.T) {
	doc := testDocument(42)
	poller, _, _, _ := wavePoller(t, serveDocument(&doc, nil))

	if got := poller.Interval(); got != 30*time.Second {
		t.Errorf("interval without failures = %v, want the chain's 30s", got)
	}
	poller.failures = 1
	if got := poller.Interval(); got != time.Minute {
		t.Errorf("interval after one failure = %v, want 1m", got)
	}
	poller.failures = 20
	if got := poller.Interval(); got != maxPollInterval {
		t.Errorf("interval after many failures = %v, want the 5m cap", got)
	}

	poller.jitter = jitterBy20Percent
	poller.failures = 0
	for i := 0; i < 50; i++ {
		got := poller.Interval()
		if got < 24*time.Second || got > 36*time.Second {
			t.Fatalf("jittered interval %v is outside ±20%% of 30s", got)
		}
	}
}

// TestJustJoinedRetriesFast: the one local exception to the single interval —
// a hop whose neighbour has not picked up the new revision yet retries every
// 5 s for three minutes, and its 404 is a state, not an error (§4.3).
func TestJustJoinedRetriesFast(t *testing.T) {
	doc := testDocument(42)
	poller, _, _, _ := wavePoller(t, serveDocument(&doc, nil))
	poller.cfg.HopSecret = "not-in-the-document-yet"

	now := time.Now()
	poller.now = func() time.Time { return now }
	poller.MarkJoined()

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Errorf("a 404 right after joining must not be an error, got %v", err)
	}
	if got := poller.Interval(); got != joinRetryInterval {
		t.Errorf("interval right after joining = %v, want %v", got, joinRetryInterval)
	}

	now = now.Add(joinRetryWindow + time.Minute)
	if err := poller.PollOnce(context.Background()); err == nil {
		t.Error("past the join window a 404 is a real failure")
	}
	if got := poller.Interval(); got == joinRetryInterval {
		t.Error("the fast retry window never closed")
	}
}

// TestReachableReflectsTheLastPoll: a next hop that answered once and then
// refuses every connection must stop reading as reachable — reachable is the
// last poll attempt's outcome, not "has this hop ever succeeded" (#98).
func TestReachableReflectsTheLastPoll(t *testing.T) {
	doc := testDocument(42)
	poller, state, _, server := wavePoller(t, serveDocument(&doc, nil))

	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if !state.Status(nil, poller.cfg).NextHop.Reachable {
		t.Fatal("reachable = false after a successful poll")
	}

	server.Close() // every later poll now fails with connection refused
	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("a poll against a closed server must fail")
	}
	if state.Status(nil, poller.cfg).NextHop.Reachable {
		t.Error("reachable = true after the next hop started refusing connections")
	}
}

// TestOuterAcksSurviveTheHeader: the acks are base64 JSON in a header, and the
// freshest report per hop wins when several routes carry one.
func TestOuterAcksSurviveTheHeader(t *testing.T) {
	state := NewState()
	state.RecordOuter(OuterAck{Name: "edge-a", LastRevision: 41, LastSeen: 100})
	state.RecordOuter(OuterAck{Name: "edge-a", LastRevision: 42, LastSeen: 200})
	state.RecordOuter(OuterAck{Name: "edge-a", LastRevision: 40, LastSeen: 50}) // older, ignored
	state.RecordOuter(OuterAck{Name: "edge-b", LastRevision: 42, LastSeen: 150})

	acks := DecodeOuterAcks(EncodeOuterAcks(state.OuterAcks()))
	if len(acks) != 2 {
		t.Fatalf("acks = %+v, want one per hop", acks)
	}
	if acks[0].Name != "edge-a" || acks[0].LastRevision != 42 || acks[0].LastSeen != 200 {
		t.Errorf("edge-a's ack = %+v, want the freshest one", acks[0])
	}
	if DecodeOuterAcks("not base64 at all") != nil {
		t.Error("a malformed header must decode to nothing, not to an error")
	}
}

// TestDocumentStoreRoundTrip: the cached document is what a rebooted box
// relays from before the first poll comes back.
func TestDocumentStoreRoundTrip(t *testing.T) {
	store := NewDocumentStore(filepath.Join(t.TempDir(), "chain", "document.json"))
	if doc, err := store.Load(); doc != nil || err != nil {
		t.Fatalf("a missing document must be (nil, nil), got %v %v", doc, err)
	}
	if err := store.Save(testDocument(42)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := store.Load()
	if err != nil || back == nil || back.Revision != 42 || len(back.Ports) != 1 {
		t.Fatalf("Load = %+v, %v", back, err)
	}
}

// TestDocumentStoreRefusesANewerVersion: half-applying a format this build
// does not know is worse than relaying the previous revision (§3.1).
func TestDocumentStoreRefusesANewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.json")
	if err := os.WriteFile(path, []byte(`{"version":9,"revision":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDocumentStore(path).Load(); err == nil {
		t.Fatal("a document from the future was accepted")
	}
}
