package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// The join forwarding limits of §4.3.
const (
	maxJoinBytes    = 4 << 10
	maxJoinForwards = 16
)

// ChainHandler serves the wave on this hop's sub port (§3.3): the truncated
// document its direct outer neighbours poll, the status its owner reads, and
// the join requests it forwards inward without understanding them.
//
// It is the mirror image of Poller: what the hop learns from its next hop it
// passes outward, cut down to what each neighbour is allowed to know.
type ChainHandler struct {
	cfg   *Config
	state *State
	relay RelayController

	client *http.Client
	now    func() time.Time
}

// NewChainHandler builds the hop's /chain/v1 endpoints.
func NewChainHandler(cfg *Config, state *State, relay RelayController) *ChainHandler {
	return &ChainHandler{
		cfg:   cfg,
		state: state,
		relay: relay,
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		now: time.Now,
	}
}

// Handler is the /chain/v1 mux. Everything it does not recognise is a bare
// 404, like every refusal of the wave: a scanner must not learn that the
// endpoint exists.
func (h *ChainHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ChainPathPrefix+"/document", h.handleDocument)
	mux.HandleFunc(ChainPathPrefix+"/status", h.handleStatus)
	mux.HandleFunc(ChainPathPrefix+"/join", h.handleJoin)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { bareNotFound(w) })
	return mux
}

// bareNotFound is every refusal of the wave: the status and nothing else.
// "404 page not found" would already be an answer — it would tell a scanner
// that something here reads its request (§3.3).
func bareNotFound(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }

// bearer is the presented hop secret, "" when the header is absent or malformed.
func bearer(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// outerNeighbour finds the hop a presented secret belongs to. Only a hop
// listed in this hop's own document can match, and never this hop itself: a
// hop authorises its direct outer neighbours by the secret hashes the panel
// put in the document, and never sees a secret in the clear (§3.1, §3.7).
func outerNeighbour(doc *chain.Document, secret string) (chain.Hop, bool) {
	if doc == nil || secret == "" {
		return chain.Hop{}, false
	}
	presented := chain.HashSecret(secret)
	for _, hop := range doc.Hops {
		if hop.Name == doc.Self.Name || hop.SecretHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(hop.SecretHash)) == 1 {
			return hop, true
		}
	}
	return chain.Hop{}, false
}

// handleDocument serves the caller its own truncated document.
func (h *ChainHandler) handleDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		bareNotFound(w)
		return
	}
	doc := h.state.Document()
	hop, ok := outerNeighbour(doc, bearer(r))
	if !ok {
		bareNotFound(w)
		return
	}

	h.recordAcks(hop, r)

	if r.Header.Get("If-None-Match") == etag(doc.Revision) {
		w.Header().Set("ETag", etag(doc.Revision))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body, err := json.Marshal(TruncateDocument(doc, hop, h.cfg))
	if err != nil {
		logger.Warning("proxy-front: chain document for", hop.Name, "could not be built:", err)
		bareNotFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("ETag", etag(doc.Revision))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// recordAcks keeps what a neighbour reports — its own revision and everything
// that reached it from further out — for this hop's own next poll inward, so
// the registry learns the whole chain's freshness without calling outward.
func (h *ChainHandler) recordAcks(hop chain.Hop, r *http.Request) {
	seen, _ := strconv.ParseInt(r.Header.Get("X-Chain-Seen"), 10, 64)
	h.state.RecordOuter(OuterAck{Name: hop.Name, LastRevision: seen, LastSeen: h.now().UnixMilli()})
	h.state.RecordOuter(DecodeOuterAcks(r.Header.Get("X-Chain-Outer"))...)
}

// TruncateDocument cuts this hop's document down to what one outer neighbour
// may know (§3.2): that neighbour and everything outward from it, this hop as
// its next hop, and nothing else inward. An edge sees only itself — a
// neighbouring edge is beside it, not outward — and learns that an active edge
// exists only when it is the active one itself.
//
// One branch belongs to a hop on its way out (§4.5.3): while self.state is
// draining, the neighbour's nextHop is this hop's own nextHop rather than this
// hop, so the neighbour re-chains past the departing box over the very channel
// that is about to close. It is the whole box-side rule of draining, and the
// neighbour learns nothing new from it: that address is the one the panel
// would have given it anyway.
func TruncateDocument(doc *chain.Document, hop chain.Hop, cfg *Config) chain.Document {
	out := chain.Document{
		Version:     chain.DocumentVersion,
		Revision:    doc.Revision,
		GeneratedAt: doc.GeneratedAt,
		Self:        chain.Self{Name: hop.Name, Role: hop.Role, Host: hop.Host, State: hop.State},
		NextHop: chain.NextHop{
			Host:      selfHost(doc, cfg),
			SubPort:   cfg.SubPort,
			SubScheme: cfg.Scheme(),
			SubPath:   doc.NextHop.SubPath,
			JsonPath:  doc.NextHop.JsonPath,
			TunPath:   doc.NextHop.TunPath,
		},
		Ports: doc.Ports,
	}
	if doc.Self.Draining() {
		out.NextHop = doc.NextHop
	}

	if hop.Role == chain.RoleEdge {
		out.Hops = []chain.Hop{hop}
		if doc.ActiveEdge == hop.Name {
			out.ActiveEdge = hop.Name
		}
		return out
	}

	out.ActiveEdge = doc.ActiveEdge
	for i, listed := range doc.Hops {
		if listed.Name == hop.Name {
			out.Hops = append([]chain.Hop(nil), doc.Hops[i:]...)
			break
		}
	}
	return out
}

// selfHost is the address an outer neighbour reaches this hop at: what the
// owner configured here, else what the registry believes.
func selfHost(doc *chain.Document, cfg *Config) string {
	if cfg.Domain != "" {
		return cfg.Domain
	}
	return doc.Self.Host
}

// handleStatus answers the owner (locally, with this hop's own secret) or a
// direct outer neighbour. It carries neither secrets nor the hop list.
func (h *ChainHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		bareNotFound(w)
		return
	}
	secret := bearer(r)
	own := secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(h.cfg.HopSecret)) == 1
	if _, neighbour := outerNeighbour(h.state.Document(), secret); !own && !neighbour {
		bareNotFound(w)
		return
	}
	body, err := json.Marshal(h.state.Status(h.relay, h.cfg))
	if err != nil {
		bareNotFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// handleJoin forwards a join inward verbatim (§4.3). The hop does not read the
// request beyond its size and the presence of a token: the panel is the only
// judge of a join token, and a neighbour is only transport.
func (h *ChainHandler) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		bareNotFound(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJoinBytes+1))
	if err != nil {
		bareNotFound(w)
		return
	}
	if len(body) > maxJoinBytes {
		http.Error(w, "join request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var probe struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Token == "" {
		bareNotFound(w)
		return
	}

	forwarded, _ := strconv.Atoi(r.Header.Get("X-Chain-Forwarded"))
	forwarded++
	if forwarded > maxJoinForwards {
		http.Error(w, "join_loop", http.StatusBadRequest)
		return
	}

	observed := r.Header.Get("X-Chain-Observed")
	if observed == "" {
		// This hop is the direct receiver, so it is the only one that can
		// say where the joining box actually came from (§4.4).
		observed = remoteHost(r.RemoteAddr)
	}

	status, answer, err := h.forwardJoin(r.Context(), body, observed, forwarded)
	if err != nil {
		logger.Warning("proxy-front: forwarding a join inward failed:", err)
		http.Error(w, "next hop unreachable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(answer)
}

// forwardJoin posts the untouched body to this hop's next hop and brings the
// answer back byte for byte.
func (h *ChainHandler) forwardJoin(ctx context.Context, body []byte, observed string, forwarded int) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.NextHopBase()+ChainPathPrefix+"/join", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Chain-Observed", observed)
	req.Header.Set("X-Chain-Forwarded", strconv.Itoa(forwarded))

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, answer, nil
}

// remoteHost is the address part of a RemoteAddr, without the ephemeral port.
func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
