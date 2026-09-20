package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

//go:embed joinpage.html
var joinPageHTML string

var joinTmpl = template.Must(template.New("join").Parse(joinPageHTML))

// insecureJoinWarning is shown on the page and logged next to the link when no
// certificate is in place. It is a warning, not a refusal: during an install
// this page is often the only channel its owner has, and refusing to serve it
// would leave the box with no way in at all (§5.4).
const insecureJoinWarning = "This page is served over plain HTTP: the join token travels in clear text. " +
	"Prefer installing the box with the token over ssh (x-ui chain rejoin --next-hop … --token …)."

// JoinResult is the panel's answer to POST /chain/v1/join (§4.3). It is the
// only moment a hop secret exists in the clear anywhere but this box.
type JoinResult struct {
	HopId       int64          `json:"hopId"`
	Name        string         `json:"name"`
	Secret      string         `json:"secret"`
	PollSeconds int            `json:"pollSeconds"`
	Document    chain.Document `json:"document"`
}

// joinRequest is the body a joining box sends inward. It travels verbatim
// through every hop on the way to the panel.
type joinRequest struct {
	Token     string `json:"token"`
	Host      string `json:"host,omitempty"`
	SubPort   int    `json:"subPort,omitempty"`
	SubScheme string `json:"subScheme,omitempty"`
}

// joinClient reaches the next hop the same way the wave does: by a hidden
// address, often a bare IP with a self-signed certificate.
func joinClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

// Join performs the join itself: POST /chain/v1/join at the next hop, which
// forwards it inward until the panel decides. A refusal is a bare 404 by
// design — an unknown, expired or spent token look alike from out here — so
// the error says all three.
func Join(ctx context.Context, cfg *Config, token string) (*JoinResult, error) {
	body, err := json.Marshal(joinRequest{
		Token:     token,
		Host:      cfg.Domain,
		SubPort:   cfg.SubPort,
		SubScheme: cfg.Scheme(),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.NextHopBase()+ChainPathPrefix+"/join", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := joinClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("next hop %s is unreachable: %w", cfg.NextHopBase(), err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return nil, fmt.Errorf("read the join answer: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("the chain refused this join token: it is unknown, expired, or already used — reissue it in the panel")
	default:
		return nil, fmt.Errorf("the chain answered %d to the join: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}

	var result JoinResult
	if err := json.Unmarshal(answer, &result); err != nil {
		return nil, fmt.Errorf("the join answer is not a join result: %w", err)
	}
	if result.Secret == "" {
		return nil, fmt.Errorf("the join answer carries no hop secret")
	}
	if result.Document.Version > chain.DocumentVersion {
		return nil, fmt.Errorf("the join answer carries document version %d, newer than this build understands (%d)",
			result.Document.Version, chain.DocumentVersion)
	}
	return &result, nil
}

// Accept records a successful join on disk and in memory: the hop secret and
// the next hop into proxy.json (0600), the document into the state dir. After
// this the box is a hop of the chain and can relay.
func Accept(cfg *Config, state *State, store *DocumentStore, result *JoinResult) error {
	cfg.HopSecret = result.Secret
	if err := cfg.Save(); err != nil {
		return err
	}
	doc := result.Document
	if err := store.Save(&doc); err != nil {
		// The document arrives again on the first poll; the secret is what
		// could not have been recovered, and it is already on disk.
		logger.Warning("proxy-front:", err)
	}
	state.SetDocument(&doc)
	logger.Infof("proxy-front: joined chain as %q (%s), next hop %s:%d, revision %d",
		doc.Self.Name, doc.Self.Role, cfg.NextHop.Host, cfg.NextHop.SubPort, doc.Revision)
	return nil
}

// JoinPage is the box's way into the chain for an owner with a browser
// (§5.4). It replaces the relay-manifest setup page of ADR 0001 and keeps its
// shape: one random token in the path, one page, gone after the first
// success. A failed attempt does not spend the token — a mistyped join token
// or an unreachable next hop must be fixable from the same page.
type JoinPage struct {
	cfg   *Config
	state *State
	store *DocumentStore

	token    string
	accepted chan struct{}

	mu     sync.Mutex
	done   bool
	result *JoinResult
}

// NewJoinPage prepares the page with a fresh one-time path token.
func NewJoinPage(cfg *Config, state *State, store *DocumentStore) (*JoinPage, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("join page token: %w", err)
	}
	return &JoinPage{
		cfg:      cfg,
		state:    state,
		store:    store,
		token:    base64.RawURLEncoding.EncodeToString(raw),
		accepted: make(chan struct{}),
	}, nil
}

// Token is the secret path segment of the join page.
func (j *JoinPage) Token() string { return j.token }

// Path is the request path of the join page.
func (j *JoinPage) Path() string { return "/join/" + j.token }

// Accepted is closed once this box has joined the chain.
func (j *JoinPage) Accepted() <-chan struct{} { return j.accepted }

// Result is what the chain answered, once it has.
func (j *JoinPage) Result() *JoinResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result
}

// Spent reports whether the page has done its one job.
func (j *JoinPage) Spent() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// URL is the address to hand the owner: the configured domain, or the
// address this host reaches the internet from, on the sub port.
func (j *JoinPage) URL() string {
	host := j.cfg.Domain
	if host == "" {
		host = outboundIP()
	}
	return j.cfg.Scheme() + "://" + net.JoinHostPort(host, strconv.Itoa(j.cfg.SubPort)) + j.Path()
}

// outboundIP finds the address the host would use to reach the internet (no
// packet is sent: a UDP "connect" only picks the route).
func outboundIP() string {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return "<this-host>"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// Handler serves the join page and nothing else.
func (j *JoinPage) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(j.Path(), j.handle)
	return mux
}

func (j *JoinPage) handle(w http.ResponseWriter, r *http.Request) {
	if j.Spent() {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		j.render(w, http.StatusOK, j.pageData())
	case http.MethodPost:
		j.submit(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// joinPageData is the view model of joinpage.html.
type joinPageData struct {
	Insecure    bool
	Warning     string
	NextHopHost string
	// HostFixed is set when the next hop came from proxy.json: then the page
	// states it instead of asking for it.
	HostFixed bool
	SubPort   int
	SubScheme string

	Error string

	Done     bool
	Name     string
	Role     string
	Revision int64
	Ports    string
}

func (j *JoinPage) pageData() joinPageData {
	return joinPageData{
		Insecure:    !j.cfg.TLS(),
		Warning:     insecureJoinWarning,
		NextHopHost: j.cfg.NextHopHint(),
		HostFixed:   j.cfg.NextHop.Host != "",
		SubPort:     j.cfg.NextHop.SubPort,
		SubScheme:   j.cfg.NextHop.SubScheme,
	}
}

// submit reads the form, joins, and — on success — starts this box as a hop.
func (j *JoinPage) submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		j.fail(w, http.StatusBadRequest, "bad form: "+err.Error())
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		j.fail(w, http.StatusBadRequest, "the join token is required — the panel shows it once, when the hop is created")
		return
	}

	// The host is only asked for when proxy.json does not already name one.
	if j.cfg.NextHop.Host == "" {
		host := strings.TrimSpace(r.FormValue("nextHopHost"))
		if host == "" {
			j.fail(w, http.StatusBadRequest, "the next hop's address is required")
			return
		}
		j.cfg.NextHop.Host = host
	}
	if raw := strings.TrimSpace(r.FormValue("nextHopSubPort")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			j.fail(w, http.StatusBadRequest, "the next hop's sub port must be 1-65535")
			return
		}
		j.cfg.NextHop.SubPort = port
	}
	if scheme := strings.TrimSpace(r.FormValue("nextHopScheme")); scheme == "http" || scheme == "https" {
		j.cfg.NextHop.SubScheme = scheme
	}

	result, err := Join(r.Context(), j.cfg, token)
	if err != nil {
		logger.Warning("proxy-front: join page:", err)
		j.fail(w, http.StatusBadGateway, err.Error())
		return
	}

	j.mu.Lock()
	if j.done { // lost a race with a concurrent submit
		j.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if err := Accept(j.cfg, j.state, j.store, result); err != nil {
		j.mu.Unlock()
		j.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	j.done = true
	j.result = result
	close(j.accepted)
	j.mu.Unlock()

	data := j.pageData()
	data.Done = true
	data.Name = result.Document.Self.Name
	data.Role = result.Document.Self.Role
	data.Revision = result.Document.Revision
	data.Ports = portSummary(result.Document.Ports)
	j.render(w, http.StatusOK, data)
}

// portSummary renders the relayed ports for the page: "443/tcp,udp, 51820/udp".
func portSummary(ports []chain.Port) string {
	listed := make([]string, 0, len(ports))
	for _, port := range ports {
		listed = append(listed, strconv.Itoa(port.Port)+"/"+port.Network)
	}
	return strings.Join(listed, ", ")
}

func (j *JoinPage) fail(w http.ResponseWriter, status int, message string) {
	data := j.pageData()
	data.Error = message
	j.render(w, status, data)
}

func (j *JoinPage) render(w http.ResponseWriter, status int, data joinPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := joinTmpl.Execute(w, data); err != nil {
		logger.Warning("proxy-front: join page render:", err)
	}
}

// JoinURLPath is where the join page's URL is kept while the page is alive,
// for the installer and `x-ui chain join-url` to print: next to the config.
func JoinURLPath(proxyConfigPath string) string {
	return filepath.Join(filepath.Dir(proxyConfigPath), "chain-join.url")
}
