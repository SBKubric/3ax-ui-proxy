package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// The wave's fixed shape (§3.3, §3.6, §4.3).
const (
	// ChainPathPrefix is not configurable: a hop must find the wave on its
	// next hop before it has any document to tell it where to look.
	ChainPathPrefix = "/chain/v1"

	// maxPollInterval caps the exponential backoff of an unreachable next
	// hop. The hop keeps relaying throughout.
	maxPollInterval = 5 * time.Minute

	// joinRetryInterval and joinRetryWindow are the one local exception to
	// the single poll interval: a hop that has just joined polls every 5 s
	// for 3 minutes, because its next hop has not picked up the new revision
	// yet and answers 404 (§4.3).
	joinRetryInterval = 5 * time.Second
	joinRetryWindow   = 3 * time.Minute

	// staleLogInterval keeps a stale hop from filling the log: one line an
	// hour, not one per poll (§3.6).
	staleLogInterval = time.Hour

	// maxOuterHeader bounds X-Chain-Outer; a longer list is cut and logged.
	maxOuterHeader = 8 << 10

	// maxDocumentBytes bounds what a next hop may hand back as a document.
	maxDocumentBytes = 4 << 20
)

// errUnauthorized is the bare 404 of the wave: this hop is not (yet, or any
// more) a direct outer neighbour of the hop it polls. It is a state, not a
// crash — a hop that has just joined sees it until the next revision reaches
// its neighbour, and a revoked hop sees it forever.
var errUnauthorized = fmt.Errorf("next hop does not know this hop's secret (404)")

// RelayController is what the wave does to the relay when a revision changes
// something the relay carries. The poller owns no xray: it hands a port list
// and a next hop over, and a test hands the same to a recorder.
type RelayController interface {
	// Apply makes the relay match these ports and this next hop, restarting
	// it (dokodemo-door has no soft reload).
	Apply(ports []chain.Port, nextHopHost string) error
	Running() bool
	Ports() []int
	RestartedAt() int64
}

// Poller is the box's half of the wave (§3.3): it polls the next hop for this
// hop's truncated chain document, caches every accepted revision and applies
// what changed. It never gives up and never switches itself off — an
// unreachable next hop only means the last known revision keeps being relayed.
type Poller struct {
	cfg   *Config
	state *State
	store *DocumentStore
	relay RelayController

	client *http.Client

	// now and jitter are injected so tests can drive the clock and get a
	// deterministic interval.
	now    func() time.Time
	jitter func(time.Duration) time.Duration

	pollSeconds  int       // what the chain says; cfg.PollSeconds overrides
	joinedAt     time.Time // zero unless this process performed the join
	failures     int
	lastStaleLog time.Time
}

// NewPoller builds the wave client for cfg. The HTTP client skips TLS
// verification for exactly the reason the subscription fetch does: a hop
// reaches its next hop by a hidden address, often a bare IP with a self-signed
// certificate, and trust here rests on the hop secret, not on the PKI.
func NewPoller(cfg *Config, state *State, store *DocumentStore, relay RelayController) *Poller {
	return &Poller{
		cfg:   cfg,
		state: state,
		store: store,
		relay: relay,
		client: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		now:         time.Now,
		jitter:      jitterBy20Percent,
		pollSeconds: DefaultPollSeconds,
	}
}

// SetPollSeconds records the interval the chain asked for (the join response's
// pollSeconds). A local pollSeconds in proxy.json still wins.
func (p *Poller) SetPollSeconds(seconds int) {
	if seconds > 0 {
		p.pollSeconds = seconds
	}
}

// MarkJoined starts the 3-minute fast-retry window of §4.3.
func (p *Poller) MarkJoined() { p.joinedAt = p.now() }

// jitterBy20Percent spreads the chain's polls so the whole chain does not call
// inward in one synchronised wave (§3.3).
func jitterBy20Percent(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * 0.2
	return time.Duration(float64(d) + (rand.Float64()*2-1)*spread)
}

// Interval is how long to wait before the next poll: the chain's interval,
// doubled per consecutive failure up to five minutes, or the fast retry of a
// hop that has just joined — all with ±20 % jitter.
func (p *Poller) Interval() time.Duration {
	base := time.Duration(p.cfg.Poll()) * time.Second
	if p.cfg.PollSeconds <= 0 && p.pollSeconds > 0 {
		base = time.Duration(p.pollSeconds) * time.Second
	}
	if p.failures > 0 {
		if !p.joinedAt.IsZero() && p.now().Sub(p.joinedAt) < joinRetryWindow {
			return p.jitter(joinRetryInterval)
		}
		backoff := base
		for i := 0; i < p.failures && backoff < maxPollInterval; i++ {
			backoff *= 2
		}
		if backoff > maxPollInterval {
			backoff = maxPollInterval
		}
		base = backoff
	}
	return p.jitter(base)
}

// Run polls until ctx is cancelled. Errors are logged, never returned: there
// is no failure of the wave that should stop a hop from relaying.
func (p *Poller) Run(ctx context.Context) {
	for {
		if err := p.PollOnce(ctx); err != nil {
			p.logPollFailure(err)
		}
		timer := time.NewTimer(p.Interval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// logPollFailure says the same thing at most once an hour while a hop is
// stale, and once per failure while it is merely retrying.
func (p *Poller) logPollFailure(err error) {
	logger.Warningf("proxy-front: next hop %s:%d unreachable (%d attempts) — keeping revision %d: %v",
		p.cfg.NextHop.Host, p.cfg.NextHop.SubPort, p.failures, p.state.Revision(), err)
}

// PollOnce performs one GET /chain/v1/document and applies what came back.
func (p *Poller) PollOnce(ctx context.Context) error {
	now := p.now()
	req, err := p.request(ctx)
	if err != nil {
		p.fail(now)
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.fail(now)
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
		if err != nil {
			p.fail(now)
			return fmt.Errorf("read chain document: %w", err)
		}
		doc, err := ParseDocument(body)
		if err != nil {
			p.fail(now)
			return err
		}
		p.ok(now)
		return p.Apply(doc)
	case http.StatusNotModified:
		p.ok(now)
		return nil
	case http.StatusNotFound:
		p.fail(now)
		if !p.joinedAt.IsZero() && now.Sub(p.joinedAt) < joinRetryWindow {
			logger.Info("proxy-front: chain: waiting for the next hop to pick up the new revision")
			return nil
		}
		return errUnauthorized
	default:
		p.fail(now)
		return fmt.Errorf("next hop answered %d", resp.StatusCode)
	}
}

// request builds the poll: the hop's bearer, the revision it already applied,
// and the acks of everything outward from it.
func (p *Poller) request(ctx context.Context) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.NextHopBase()+ChainPathPrefix+"/document", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.HopSecret)
	revision := p.state.Revision()
	if revision > 0 {
		req.Header.Set("If-None-Match", etag(revision))
		req.Header.Set("X-Chain-Seen", strconv.FormatInt(revision, 10))
	}
	if header := EncodeOuterAcks(p.state.OuterAcks()); header != "" {
		req.Header.Set("X-Chain-Outer", header)
	}
	return req, nil
}

// etag is the document's ETag: the revision in quotes (§3.4).
func etag(revision int64) string { return `"` + strconv.FormatInt(revision, 10) + `"` }

// EncodeOuterAcks renders the X-Chain-Outer header: base64 JSON, cut to
// maxOuterHeader by dropping the oldest acks rather than sending a body a
// proxy may swallow (§3.3).
func EncodeOuterAcks(acks []OuterAck) string {
	for len(acks) > 0 {
		data, err := json.Marshal(acks)
		if err != nil {
			return ""
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		if len(encoded) <= maxOuterHeader {
			return encoded
		}
		logger.Warningf("proxy-front: X-Chain-Outer over %d bytes — dropping the oldest ack of %d", maxOuterHeader, len(acks))
		acks = acks[1:]
	}
	return ""
}

// DecodeOuterAcks reads an X-Chain-Outer header. A malformed header is worth
// no more than an empty one: the acks are freshness hints for the registry,
// and a hop that garbles them must not break the poll that carries them.
func DecodeOuterAcks(header string) []OuterAck {
	if header == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(header)
	if err != nil || len(data) > maxOuterHeader {
		return nil
	}
	var acks []OuterAck
	if err := json.Unmarshal(data, &acks); err != nil {
		return nil
	}
	return acks
}

func (p *Poller) ok(now time.Time) {
	p.failures = 0
	p.state.MarkPoll(now.UnixMilli(), true)
}

// fail records a failed poll and, past staleMinutes without a 200 or a 304,
// marks the hop stale — while it keeps relaying the last revision it has,
// with no upper limit on staleness (§3.6).
func (p *Poller) fail(now time.Time) {
	p.failures++
	p.state.MarkPoll(now.UnixMilli(), false)

	lastOk := p.state.LastOk()
	if lastOk == 0 {
		return
	}
	silent := now.Sub(time.UnixMilli(lastOk))
	if silent < time.Duration(p.cfg.StaleMinutes)*time.Minute {
		return
	}
	p.state.SetStale(true)
	if now.Sub(p.lastStaleLog) < staleLogInterval {
		return
	}
	p.lastStaleLog = now
	logger.Warningf("proxy-front: chain document is stale (%dm, last ok %s, next hop %s) — still relaying revision %d",
		int(silent.Minutes()), time.UnixMilli(lastOk).Format(time.RFC3339), p.cfg.NextHop.Host, p.state.Revision())
}

// Apply stores a freshly received document and does exactly what changed
// (§3.5). The document is written before anything is applied and kept even if
// applying fails: a hop that dropped it would ask for it again, fail again and
// loop, while a hop that keeps it simply retries on the next revision.
func (p *Poller) Apply(doc *chain.Document) error {
	previous := p.state.Document()
	if previous != nil && previous.Revision == doc.Revision && !documentDiffers(previous, doc) {
		return nil
	}

	if err := p.store.Save(doc); err != nil {
		// Still apply: a hop that relays the new revision but forgot it will
		// re-learn it on the next poll; one that refuses both is just dark.
		logger.Warning("proxy-front:", err)
	}
	p.state.SetDocument(doc)

	added, removed := portDiff(previous, doc)
	needRelay := previous == nil || added != 0 || removed != 0 ||
		previous.NextHop.Host != doc.NextHop.Host || !samePorts(previous.Ports, doc.Ports)

	if secretsDiffer(previous, doc) {
		// The authorisation table for /chain/v1/* is read straight from the
		// current document, so a changed set of hashes needs no restart —
		// only a word in the log, since it is how a revoked neighbour stops
		// being served.
		logger.Infof("proxy-front: chain revision %d: the outer neighbours' secrets changed — auth table reloaded", doc.Revision)
	}

	if !needRelay {
		logger.Infof("proxy-front: chain revision %d applied (no relay change)", doc.Revision)
		return nil
	}
	if err := p.relay.Apply(doc.Ports, doc.NextHop.Host); err != nil {
		return fmt.Errorf("apply chain revision %d: %w", doc.Revision, err)
	}
	logger.Infof("proxy-front: chain revision %d applied (ports +%d -%d)", doc.Revision, added, removed)
	return nil
}

// documentDiffers reports whether anything a hop acts on changed, ignoring
// generatedAt: a re-sent identical revision must not restart a relay.
func documentDiffers(a, b *chain.Document) bool {
	return !samePorts(a.Ports, b.Ports) ||
		a.NextHop != b.NextHop ||
		a.Self != b.Self ||
		a.ActiveEdge != b.ActiveEdge ||
		secretsDiffer(a, b)
}

// samePorts compares two port lists as sets: order is the panel's business.
func samePorts(a, b []chain.Port) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[int]chain.Port, len(a))
	for _, port := range a {
		index[port.Port] = port
	}
	for _, port := range b {
		if prev, found := index[port.Port]; !found || prev != port {
			return false
		}
	}
	return true
}

// portDiff counts what a revision adds and removes, for the log line of §5.5.
func portDiff(previous, next *chain.Document) (added, removed int) {
	before := make(map[int]bool)
	if previous != nil {
		for _, port := range previous.Ports {
			before[port.Port] = true
		}
	}
	after := make(map[int]bool, len(next.Ports))
	for _, port := range next.Ports {
		after[port.Port] = true
		if !before[port.Port] {
			added++
		}
	}
	for port := range before {
		if !after[port] {
			removed++
		}
	}
	return added, removed
}

// secretsDiffer reports whether the set of hops, or any hop's secret hash,
// changed — the one thing that decides who this hop serves documents to.
func secretsDiffer(previous, next *chain.Document) bool {
	if previous == nil {
		return len(next.Hops) > 0
	}
	if len(previous.Hops) != len(next.Hops) {
		return true
	}
	before := make(map[string]string, len(previous.Hops))
	for _, hop := range previous.Hops {
		before[hop.Name] = hop.SecretHash
	}
	for _, hop := range next.Hops {
		if hash, found := before[hop.Name]; !found || hash != hop.SecretHash {
			return true
		}
	}
	return false
}
