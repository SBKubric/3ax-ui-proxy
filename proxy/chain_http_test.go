package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// innerDocument is what an inner hop holds: itself and everything outward —
// the two edges beside each other and the inner outward of it.
func innerDocument() *chain.Document {
	return &chain.Document{
		Version:     chain.DocumentVersion,
		Revision:    42,
		GeneratedAt: 1758380000000,
		Self:        chain.Self{Name: "inner-1", Role: chain.RoleInner, Host: "10.0.0.7"},
		NextHop: chain.NextHop{
			Host: "198.51.100.1", SubPort: 2096, SubScheme: "https",
			SubPath: "/sub/", JsonPath: "/json/", TunPath: "/tun/",
		},
		ActiveEdge: "edge-a",
		Hops: []chain.Hop{
			{Name: "inner-1", Role: chain.RoleInner, Host: "10.0.0.7", SubPort: 2096, SecretHash: chain.HashSecret("inner-1-secret"), State: chain.StateJoined},
			{Name: "inner-2", Role: chain.RoleInner, Host: "203.0.113.9", SubPort: 2096, SecretHash: chain.HashSecret("inner-2-secret"), State: chain.StateJoined},
			{Name: "edge-a", Role: chain.RoleEdge, Host: "a.example.net", SubPort: 2096, SecretHash: chain.HashSecret("edge-a-secret"), State: chain.StateJoined},
			{Name: "edge-b", Role: chain.RoleEdge, Host: "b.example.net", SubPort: 2096, SecretHash: chain.HashSecret("edge-b-secret"), State: chain.StateJoined},
		},
		Ports: []chain.Port{{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray}},
	}
}

func testChainHandler(t *testing.T, cfg *Config, state *State) http.Handler {
	t.Helper()
	if cfg.SubPort == 0 {
		cfg.SubPort = DefaultSubPort
	}
	if cfg.NextHop.SubPort == 0 {
		cfg.NextHop.SubPort = DefaultSubPort
	}
	if cfg.NextHop.SubScheme == "" {
		cfg.NextHop.SubScheme = DefaultSubScheme
	}
	return NewChainHandler(cfg, state, &recordingRelay{}).Handler()
}

func getWithSecret(h http.Handler, path, secret string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestDocumentIsTruncatedPerNeighbour is the heart of §3.2: an outer
// neighbour sees itself and what is outward of it, this hop as its next hop,
// and nothing inward — and an edge does not learn that a sibling edge exists.
func TestDocumentIsTruncatedPerNeighbour(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	cfg := &Config{Domain: "inner-1.example.net", SubPort: 2096, CertFile: "c", KeyFile: "k"}
	h := testChainHandler(t, cfg, state)

	w := getWithSecret(h, ChainPathPrefix+"/document", "inner-2-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("inner-2's document: status %d", w.Code)
	}
	if got := w.Header().Get("ETag"); got != `"42"` {
		t.Errorf("ETag = %q, want the revision", got)
	}
	var doc chain.Document
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document: %v", err)
	}
	if doc.Self.Name != "inner-2" {
		t.Errorf("self = %+v, want inner-2", doc.Self)
	}
	if doc.NextHop.Host != "inner-1.example.net" || doc.NextHop.SubScheme != "https" || doc.NextHop.SubPort != 2096 {
		t.Errorf("nextHop = %+v, want this hop", doc.NextHop)
	}
	if doc.NextHop.SubPath != "/sub/" || doc.NextHop.TunPath != "/tun/" {
		t.Errorf("subscription paths were not passed outward: %+v", doc.NextHop)
	}
	names := hopNames(doc.Hops)
	if strings.Join(names, ",") != "inner-2,edge-a,edge-b" {
		t.Errorf("hops = %v, want inner-2 and everything outward", names)
	}
	if doc.ActiveEdge != "edge-a" {
		t.Errorf("an inner must learn the active edge, got %q", doc.ActiveEdge)
	}
	for _, hop := range doc.Hops {
		if hop.Name == "inner-1" {
			t.Error("the document leaked this hop itself to an outer neighbour")
		}
	}

	// An edge sees only itself: a neighbouring edge is beside it, not
	// outward — that is the hint truncation exists to withhold.
	w = getWithSecret(h, ChainPathPrefix+"/document", "edge-b-secret", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("edge-b's document: status %d", w.Code)
	}
	doc = chain.Document{}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if names := hopNames(doc.Hops); len(names) != 1 || names[0] != "edge-b" {
		t.Errorf("edge-b's hops = %v, want only itself", names)
	}
	if doc.ActiveEdge != "" {
		t.Errorf("a standby edge learned the active edge's name (%q)", doc.ActiveEdge)
	}

	// The active edge may know it is the active one.
	w = getWithSecret(h, ChainPathPrefix+"/document", "edge-a-secret", nil)
	doc = chain.Document{}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ActiveEdge != "edge-a" {
		t.Errorf("the active edge's activeEdge = %q, want its own name", doc.ActiveEdge)
	}
}

func hopNames(hops []chain.Hop) []string {
	names := make([]string, 0, len(hops))
	for _, hop := range hops {
		names = append(names, hop.Name)
	}
	return names
}

// TestDocumentRefusesEveryoneElseWithABare404: an unknown secret, a missing
// header and this hop's own secret all look alike from outside — a scanner
// must not learn that the endpoint is there (§3.3).
func TestDocumentRefusesEveryoneElseWithABare404(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	h := testChainHandler(t, &Config{Domain: "inner-1.example.net"}, state)

	for _, secret := range []string{"", "wrong", "inner-1-secret"} {
		w := getWithSecret(h, ChainPathPrefix+"/document", secret, nil)
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("secret %q: status %d body %q, want a bare 404", secret, w.Code, w.Body.String())
		}
	}

	// A hop with no document of its own can serve nobody.
	empty := testChainHandler(t, &Config{}, NewState())
	if w := getWithSecret(empty, ChainPathPrefix+"/document", "edge-a-secret", nil); w.Code != http.StatusNotFound {
		t.Errorf("a hop without a document answered %d", w.Code)
	}
}

// TestDocument304AndAckRecording: an unchanged revision costs a header, and
// the caller's ack travels on inward on this hop's own next poll (§3.3).
func TestDocument304AndAckRecording(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	h := testChainHandler(t, &Config{Domain: "inner-1.example.net"}, state)

	outer, _ := json.Marshal([]OuterAck{{Name: "edge-a", LastRevision: 42, LastSeen: 1758379990000}})
	w := getWithSecret(h, ChainPathPrefix+"/document", "inner-2-secret", map[string]string{
		"If-None-Match": `"42"`,
		"X-Chain-Seen":  "42",
		"X-Chain-Outer": EncodeOuterAcks([]OuterAck{{Name: "edge-a", LastRevision: 42, LastSeen: 1758379990000}}),
	})
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("matching If-None-Match: status %d body %q, want a bodiless 304", w.Code, w.Body.String())
	}
	_ = outer

	acks := state.OuterAcks()
	if len(acks) != 2 {
		t.Fatalf("acks = %+v, want the caller's and the one it carried", acks)
	}
	byName := map[string]OuterAck{}
	for _, ack := range acks {
		byName[ack.Name] = ack
	}
	if byName["inner-2"].LastRevision != 42 {
		t.Errorf("the caller's own ack was not recorded: %+v", byName["inner-2"])
	}
	if byName["edge-a"].LastRevision != 42 {
		t.Errorf("an ack from further out was not passed on: %+v", byName["edge-a"])
	}

	// A stale If-None-Match gets the document.
	w = getWithSecret(h, ChainPathPrefix+"/document", "inner-2-secret", map[string]string{"If-None-Match": `"41"`})
	if w.Code != http.StatusOK {
		t.Errorf("an outdated ETag: status %d, want the document", w.Code)
	}
}

// TestStatusAnswersTheOwnerAndTheNeighbourOnly (§3.6).
func TestStatusAnswersTheOwnerAndTheNeighbourOnly(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	state.MarkPoll(1758379990000, true)
	cfg := &Config{HopSecret: "inner-1-secret", Domain: "10.0.0.7"}
	h := testChainHandler(t, cfg, state)

	for _, secret := range []string{"inner-1-secret", "edge-a-secret"} {
		w := getWithSecret(h, ChainPathPrefix+"/status", secret, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("secret %q: status %d", secret, w.Code)
		}
		var status chain.Status
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatalf("status body: %v", err)
		}
		if status.Name != "inner-1" || status.Role != chain.RoleInner || status.Revision != 42 {
			t.Errorf("status = %+v", status)
		}
		if status.NextHop.Host != "198.51.100.1" || !status.NextHop.Reachable {
			t.Errorf("status.nextHop = %+v", status.NextHop)
		}
		if strings.Contains(w.Body.String(), "secretHash") || strings.Contains(w.Body.String(), "hops") {
			t.Error("the status leaked the hop list or a secret hash")
		}
	}
	if w := getWithSecret(h, ChainPathPrefix+"/status", "nobody", nil); w.Code != http.StatusNotFound {
		t.Errorf("an unknown secret got status %d, want 404", w.Code)
	}
}

// TestStatusReportsAHostMismatch: the registry's address for this hop against
// the one its owner configured here — a hint, never a refusal (§4.4).
func TestStatusReportsAHostMismatch(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	cfg := &Config{HopSecret: "inner-1-secret", Domain: "somewhere.else.example"}
	h := testChainHandler(t, cfg, state)

	w := getWithSecret(h, ChainPathPrefix+"/status", "inner-1-secret", nil)
	var status chain.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.ObservedHostMismatch {
		t.Error("a host that differs from the registry's was not reported")
	}
}

// TestJoinIsForwardedInwardVerbatim (§4.3): a hop is transport, not a judge.
// It adds where the request came from, counts the hops it crossed, and hands
// the answer back byte for byte.
func TestJoinIsForwardedInwardVerbatim(t *testing.T) {
	var gotBody string
	var gotHeaders http.Header
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody, gotHeaders = string(body), r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hopId":4,"name":"edge-b","secret":"s","pollSeconds":30,"document":{"version":1,"revision":43}}`))
	}))
	defer inner.Close()

	host, port := hostPort(t, inner.URL)
	h := testChainHandler(t, &Config{NextHop: NextHop{Host: host, SubPort: port, SubScheme: "http"}}, NewState())

	request := `{"token":"0123456789012345678901234567890a","host":"b.example.net","subPort":2096,"subScheme":"https"}`
	req := httptest.NewRequest(http.MethodPost, ChainPathPrefix+"/join", strings.NewReader(request))
	req.RemoteAddr = "198.51.100.44:51234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"edge-b"`) {
		t.Fatalf("join answer: status %d body %q", w.Code, w.Body.String())
	}
	if gotBody != request {
		t.Errorf("forwarded body = %q, want it verbatim", gotBody)
	}
	if got := gotHeaders.Get("X-Chain-Observed"); got != "198.51.100.44" {
		t.Errorf("X-Chain-Observed = %q, want the joining box's address", got)
	}
	if got := gotHeaders.Get("X-Chain-Forwarded"); got != "1" {
		t.Errorf("X-Chain-Forwarded = %q, want 1", got)
	}

	// A hop further out already saw the box: its observation is the one that
	// counts, and the counter keeps climbing.
	req = httptest.NewRequest(http.MethodPost, ChainPathPrefix+"/join", strings.NewReader(request))
	req.Header.Set("X-Chain-Observed", "203.0.113.5")
	req.Header.Set("X-Chain-Forwarded", "3")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := gotHeaders.Get("X-Chain-Observed"); got != "203.0.113.5" {
		t.Errorf("X-Chain-Observed = %q, want the first receiver's observation", got)
	}
	if got := gotHeaders.Get("X-Chain-Forwarded"); got != "4" {
		t.Errorf("X-Chain-Forwarded = %q, want 4", got)
	}
}

// TestJoinRefusesALoopAndJunk: a request that has crossed 16 hops is a loop,
// a body without a token is not a join, and an oversized one is refused
// before it is read (§4.3).
func TestJoinRefusesALoopAndJunk(t *testing.T) {
	h := testChainHandler(t, &Config{NextHop: NextHop{Host: "127.0.0.1", SubPort: 1, SubScheme: "http"}}, NewState())
	body := `{"token":"0123456789012345678901234567890a"}`

	req := httptest.NewRequest(http.MethodPost, ChainPathPrefix+"/join", strings.NewReader(body))
	req.Header.Set("X-Chain-Forwarded", "16")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "join_loop") {
		t.Errorf("a 17th forward: status %d body %q, want 400 join_loop", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ChainPathPrefix+"/join", strings.NewReader(`{"nope":1}`)))
	if w.Code != http.StatusNotFound {
		t.Errorf("a body without a token: status %d, want a bare 404", w.Code)
	}

	huge := `{"token":"` + strings.Repeat("x", maxJoinBytes) + `"}`
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ChainPathPrefix+"/join", strings.NewReader(huge)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized join: status %d, want 413", w.Code)
	}
}
