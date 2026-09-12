package proxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/relaymanifest"
)

// maxManifestBytes bounds a paste; a real manifest is a few KB.
const maxManifestBytes = 1 << 20

// SetupServer is the proxy front's bootstrap mode: while no relay manifest
// exists, the process serves nothing but a one-time setup page on the
// subscription port, reachable only under a random token. The owner pastes
// the manifest exported from the real panel; once one is accepted it is
// written to Config.RelayManifestPath, Accepted() fires, and the page goes
// dark — the token is spent, and every later request is a 404.
type SetupServer struct {
	cfg        *Config
	token      string
	accepted   chan struct{}
	httpServer *http.Server

	mu   sync.Mutex
	done bool
}

// NewSetupServer prepares a setup server with a fresh token.
func NewSetupServer(cfg *Config) (*SetupServer, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("setup token: %w", err)
	}
	return &SetupServer{
		cfg:      cfg,
		token:    base64.RawURLEncoding.EncodeToString(raw),
		accepted: make(chan struct{}),
	}, nil
}

// Token is the secret path segment of the setup page.
func (s *SetupServer) Token() string { return s.token }

// Accepted is closed once a valid manifest has been written.
func (s *SetupServer) Accepted() <-chan struct{} { return s.accepted }

// Path is the request path of the setup page.
func (s *SetupServer) Path() string { return "/setup/" + s.token }

// URL is the full address to hand the owner: the configured Domain (or the
// host's outbound address when none is set) on the subscription port, with
// the scheme the subscription server itself will use.
func (s *SetupServer) URL() string {
	scheme := "http"
	if s.cfg.TLS() {
		scheme = "https"
	}
	host := s.cfg.Domain
	if host == "" {
		host = outboundIP()
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(s.cfg.SubPort)) + s.Path()
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

// Handler serves the setup page and nothing else.
func (s *SetupServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.Path(), s.handle)
	return mux
}

func (s *SetupServer) spent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

func (s *SetupServer) handle(w http.ResponseWriter, r *http.Request) {
	if s.spent() {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.render(w, http.StatusOK, setupPageData{Error: ""})
	case http.MethodPost:
		s.accept(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// accept validates a pasted manifest (form field "manifest", or the raw body
// for curl), writes it and fires Accepted. A bad paste is a 400 with the
// validator's reason; the page stays up for another try.
func (s *SetupServer) accept(w http.ResponseWriter, r *http.Request) {
	var body string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") ||
		strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxManifestBytes)
		if err := r.ParseMultipartForm(maxManifestBytes); err != nil && err != http.ErrNotMultipart {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		body = r.FormValue("manifest")
	} else {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		body = string(raw)
	}

	m, err := relaymanifest.Validate([]byte(body))
	if err != nil {
		logger.Warningf("proxy-front: setup page rejected a paste: %v", err)
		s.render(w, http.StatusBadRequest, setupPageData{Error: err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done { // lost a race with a concurrent paste
		http.NotFound(w, r)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.RelayManifestPath), 0o755); err != nil {
		http.Error(w, "store manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(s.cfg.RelayManifestPath, []byte(body), 0o600); err != nil {
		http.Error(w, "store manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.done = true
	close(s.accepted)

	ports := make([]string, 0, len(m.Inbounds))
	for _, in := range m.Inbounds {
		if skipReason(in) == "" {
			ports = append(ports, strconv.Itoa(in.Port))
		}
	}
	for _, ep := range s.cfg.ExtraRelayPorts() {
		ports = append(ports, strconv.Itoa(ep.Port)+"/"+ep.Network)
	}
	logger.Infof("proxy-front: relay manifest accepted via setup page, written to %s", s.cfg.RelayManifestPath)
	s.render(w, http.StatusOK, setupPageData{Done: true, Ports: strings.Join(ports, ", "), Upstream: s.cfg.UpstreamHost})
}

type setupPageData struct {
	Error    string
	Done     bool
	Ports    string
	Upstream string
}

func (s *SetupServer) render(w http.ResponseWriter, status int, d setupPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := setupTmpl.Execute(w, d); err != nil {
		logger.Warning("proxy-front: setup page render:", err)
	}
}

// Start listens on the subscription address and serves the page; the
// subscription server takes the port over once Stop has run.
func (s *SetupServer) Start() error {
	addr := net.JoinHostPort(s.cfg.SubListen, strconv.Itoa(s.cfg.SubPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy setup page listen %s: %w", addr, err)
	}
	s.httpServer = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		var serr error
		if s.cfg.TLS() {
			serr = s.httpServer.ServeTLS(ln, s.cfg.CertFile, s.cfg.KeyFile)
		} else {
			serr = s.httpServer.Serve(ln)
		}
		if serr != nil && serr != http.ErrServerClosed {
			logger.Error("proxy setup page:", serr)
		}
	}()
	return nil
}

// Stop drains in-flight responses (the "accepted" page) and frees the port.
func (s *SetupServer) Stop() error {
	if s.httpServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return s.httpServer.Close()
	}
	return nil
}

// SetupURLPath is where the setup URL is kept for the installer and
// `x-ui proxy-setup-url` to print: next to the proxy config.
func SetupURLPath(proxyConfigPath string) string {
	return filepath.Join(filepath.Dir(proxyConfigPath), "proxy-setup.url")
}

var setupTmpl = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Proxy front setup</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 16px;color:#222;background:#fafafa}
textarea{width:100%;min-height:320px;font:13px/1.4 ui-monospace,monospace;padding:8px;box-sizing:border-box}
button{font:inherit;padding:8px 18px;margin-top:10px}
.err{background:#fde8e8;border:1px solid #f5b5b5;padding:10px 12px;border-radius:6px;white-space:pre-wrap}
.ok{background:#e6f6e6;border:1px solid #a9d9a9;padding:10px 12px;border-radius:6px}
code{background:#eee;padding:1px 4px;border-radius:3px}
</style></head><body>
<h1>Proxy front setup</h1>
{{if .Done}}
<p class="ok">Relay manifest accepted. The relay is starting now for ports <b>{{.Ports}}</b> → <code>{{.Upstream}}</code>.
This page is gone; the subscription server takes this port over in a moment.</p>
{{else}}
<p>This box has no relay manifest yet. On the <b>real</b> panel run <code>x-ui relay-manifest</code>
(or open Settings → Subscription → <i>Show manifest</i>), copy the output and paste it here.
It lists ports only — no keys ever leave the real server.</p>
{{if .Error}}<p class="err">Rejected: {{.Error}}</p>{{end}}
<form method="post">
<textarea name="manifest" placeholder='{"relayManifest": {"version": 1, ...}, "inbounds": [...]}' required></textarea><br>
<button type="submit">Start the relay</button>
</form>
{{end}}
</body></html>
`))
