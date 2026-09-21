package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// DocumentStore is the hop's cache of the last accepted chain document
// (`<stateDir>/document.json`, §3.6). A rebooted box relays from it before the
// first poll comes back, and an unreachable next hop costs it nothing: the
// file is the hop's memory of the chain, not a mirror of a live connection.
type DocumentStore struct {
	path string
}

// NewDocumentStore points a store at a document.json path.
func NewDocumentStore(path string) *DocumentStore { return &DocumentStore{path: path} }

// Path is the file this store reads and writes.
func (s *DocumentStore) Path() string { return s.path }

// Load reads the cached document. A missing file is not an error — it is a box
// that has not received a document yet — and returns (nil, nil).
func (s *DocumentStore) Load() (*chain.Document, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read chain document %q: %w", s.path, err)
	}
	doc, err := ParseDocument(data)
	if err != nil {
		return nil, fmt.Errorf("chain document %q: %w", s.path, err)
	}
	return doc, nil
}

// Save writes the document atomically and privately (0600): it carries the
// next hop's address and every hop's secret hash.
func (s *DocumentStore) Save(doc *chain.Document) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode chain document: %w", err)
	}
	if err := writeFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write chain document %q: %w", s.path, err)
	}
	return nil
}

// ParseDocument decodes a chain document and refuses a version this build does
// not know: applying half a format it does not understand would be worse than
// relaying the previous revision (§3.1).
func ParseDocument(data []byte) (*chain.Document, error) {
	var doc chain.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not a chain document: %w", err)
	}
	if doc.Version > chain.DocumentVersion {
		return nil, fmt.Errorf("document version %d is newer than this build understands (%d)", doc.Version, chain.DocumentVersion)
	}
	return &doc, nil
}

// OuterAck is one outer neighbour's acknowledgement, as it travels inward in
// the X-Chain-Outer header (§3.3). A hop reports what its own outer
// neighbours told it, so the registry learns the freshness of every hop
// without ever calling outward.
type OuterAck struct {
	Name         string `json:"name"`
	LastRevision int64  `json:"lastRevision"`
	LastSeen     int64  `json:"lastSeen"`
}

// State is everything the running hop knows about itself: the current
// document, when the wave last succeeded, and the acks its outer neighbours
// left behind. Every reader — the poller, the status endpoint, the document
// endpoint and the subscription server — goes through it, so there is one
// answer to "what revision is this box on" instead of four.
type State struct {
	mu         sync.RWMutex
	doc        *chain.Document
	lastPoll   int64
	lastOk     int64
	lastPollOK bool
	stale      bool
	outer      map[string]OuterAck
}

// NewState returns an empty state — a box that has not joined yet.
func NewState() *State { return &State{outer: make(map[string]OuterAck)} }

// Document is the last accepted chain document, or nil while there is none.
func (s *State) Document() *chain.Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doc
}

// SetDocument replaces the current document.
func (s *State) SetDocument(doc *chain.Document) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc
}

// Revision is the revision this hop has applied, 0 while it has no document.
func (s *State) Revision() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.doc == nil {
		return 0
	}
	return s.doc.Revision
}

// MarkPoll records that a poll was attempted; ok also records success and
// clears staleness. lastPollOK always reflects this call's outcome, so the
// status endpoint's reachable flag tracks the last attempt rather than
// lingering true from some earlier success (§3.6).
func (s *State) MarkPoll(nowMilli int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPoll = nowMilli
	s.lastPollOK = ok
	if ok {
		s.lastOk = nowMilli
		s.stale = false
	}
}

// SetStale flags the hop as relaying an ageing document. It never stops
// relaying: a live relay on a slightly old revision beats a dead one (§3.6).
func (s *State) SetStale(stale bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale = stale
}

// LastOk is when the wave last returned 200 or 304, in milliseconds.
func (s *State) LastOk() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastOk
}

// RecordOuter stores the acks an outer neighbour reported: its own, plus
// everything that reached it from further out. The freshest value per hop
// wins, since the same hop can be reported by several routes.
func (s *State) RecordOuter(acks ...OuterAck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ack := range acks {
		if ack.Name == "" {
			continue
		}
		if prev, found := s.outer[ack.Name]; found && prev.LastSeen >= ack.LastSeen {
			continue
		}
		s.outer[ack.Name] = ack
	}
}

// OuterAcks is what this hop passes inward on its next poll, ordered by name
// so the header is stable between polls.
func (s *State) OuterAcks() []OuterAck {
	s.mu.RLock()
	defer s.mu.RUnlock()
	acks := make([]OuterAck, 0, len(s.outer))
	for _, ack := range s.outer {
		acks = append(acks, ack)
	}
	sort.Slice(acks, func(i, j int) bool { return acks[i].Name < acks[j].Name })
	return acks
}

// Status is the body of GET /chain/v1/status (§3.6): a health view, never a
// second way to read the document — no secrets and no hop list.
func (s *State) Status(relay RelayController, cfg *Config) chain.Status {
	s.mu.RLock()
	doc := s.doc
	lastPollOK := s.lastPollOK
	status := chain.Status{
		Version:  chain.DocumentVersion,
		LastPoll: s.lastPoll,
		LastOk:   s.lastOk,
		Stale:    s.stale,
	}
	s.mu.RUnlock()

	nextHost, nextPort := cfg.NextHop.Host, cfg.NextHop.SubPort
	if doc != nil {
		status.Name = doc.Self.Name
		status.Role = doc.Self.Role
		status.Revision = doc.Revision
		status.Draining = doc.Self.Draining()
		if doc.NextHop.Host != "" {
			nextHost, nextPort = doc.NextHop.Host, doc.NextHop.SubPort
		}
		// The registry's idea of this hop's address against the one its
		// owner configured here: a hint for the owner, never a refusal (§4.4).
		status.ObservedHostMismatch = cfg.Domain != "" && doc.Self.Host != "" && doc.Self.Host != cfg.Domain
	}
	status.NextHop = chain.StatusNextHop{
		Host:    nextHost,
		SubPort: nextPort,
		// Reachable reflects the last poll attempt, not the history of ever
		// having succeeded: a next hop that answered once and has refused
		// every connection since must not still read as reachable (#98).
		Reachable: !status.Stale && lastPollOK,
	}
	if relay != nil {
		status.Relay = chain.StatusRelay{
			Running:     relay.Running(),
			Ports:       relay.Ports(),
			RestartedAt: relay.RestartedAt(),
		}
	}
	if status.Relay.Ports == nil {
		status.Relay.Ports = []int{}
	}
	return status
}
