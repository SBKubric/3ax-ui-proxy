package proxy

import (
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/util/common"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed subpage.html
var subpageHTML string

// Fallback subscription paths, used only to tell "the client asked for a
// subscription while this hop has no document" (503) from "this is not a
// route at all" (404). The real paths always come from the document.
const (
	fallbackSubPath  = "/sub/"
	fallbackJsonPath = "/json/"
)

// app is one recommended client app shown on the proxy subscription page.
type app struct {
	Name     string
	Platform string
	URL      string
}

// recommendedApps is the curated client list shown on the proxy page.
var recommendedApps = []app{
	{Name: "Amnezia", Platform: "Android", URL: "https://github.com/amnezia-vpn/amnezia-client/releases"},
	{Name: "V2rayNG", Platform: "Android", URL: "https://github.com/2dust/v2rayNG/releases"},
	{Name: "DefaultVPN", Platform: "iOS", URL: "https://apps.apple.com/ru/app/defaultvpn/id6744725017"},
	{Name: "SongBird", Platform: "Windows", URL: "https://github.com/o3ku/SongBird/releases/"},
}

// headers copied through from the next hop to subscription clients.
var passthroughHeaders = []string{
	"Subscription-Userinfo", "Profile-Update-Interval", "Profile-Title",
	"Profile-Web-Page-Url", "Support-Url", "Announce", "Routing-Enable", "Routing",
}

// SubServer is the hop's sub port: subscriptions proxied from the next hop,
// the wave endpoints, and — until this box has joined — the join page. One
// port carries all three (§3.3): the hop is open to the world anyway, the
// wave is bearer-protected, and a second port would mean another field in the
// registry, the document and the installer.
//
// Which upstream it fetches from and under which paths is not configuration:
// both come from the current chain document, so a revision that moves them
// takes effect without a restart (§3.5).
type SubServer struct {
	cfg   *Config
	state *State
	chain *ChainHandler
	join  *JoinPage

	tmpl       *template.Template
	client     *http.Client
	httpServer *http.Server
}

// NewSubServer builds the hop's sub server (does not start it). join may be
// nil for a box that has already joined.
func NewSubServer(cfg *Config, state *State, chainHandler *ChainHandler, join *JoinPage) (*SubServer, error) {
	tmpl, err := template.New("subpage").Parse(subpageHTML)
	if err != nil {
		return nil, fmt.Errorf("parse proxy subpage template: %w", err)
	}
	return &SubServer{
		cfg:   cfg,
		state: state,
		chain: chainHandler,
		join:  join,
		tmpl:  tmpl,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// A hop reaches its next hop by a hidden address (often a bare
			// IP and/or a self-signed cert), so TLS verification is skipped
			// for this hop-to-hop leg; trust rests on the hop secret.
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}, nil
}

// Handler is the whole sub port, exposed for tests.
func (s *SubServer) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	if s.chain != nil {
		engine.Any(ChainPathPrefix+"/*endpoint", gin.WrapH(s.chain.Handler()))
	}
	if s.join != nil {
		engine.Any("/join/*token", gin.WrapH(s.join.Handler()))
	}
	// Subscription paths travel in the document and can change under a
	// running server, so they are matched per request rather than registered.
	engine.NoRoute(s.route)
	return engine
}

// Start binds and serves the sub port in a background goroutine.
func (s *SubServer) Start() error {
	addr := net.JoinHostPort(s.cfg.SubListen, strconv.Itoa(s.cfg.SubPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy sub server listen %s: %w", addr, err)
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
			logger.Error("proxy sub server:", serr)
		}
	}()

	logger.Infof("proxy-front: sub port %s (subscriptions, %s/*, join page)", addr, ChainPathPrefix)
	return nil
}

// Stop shuts the sub server down.
func (s *SubServer) Stop() error {
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// nextHop is where subscriptions are fetched from and under which paths: the
// document's next hop, falling back to proxy.json before the first document.
func (s *SubServer) nextHop() (base string, sub string, json string) {
	doc := s.state.Document()
	base, sub, json = s.cfg.NextHopBase(), fallbackSubPath, fallbackJsonPath
	if doc == nil {
		return base, sub, json
	}
	if doc.NextHop.Host != "" {
		scheme := doc.NextHop.SubScheme
		if scheme == "" {
			scheme = DefaultSubScheme
		}
		port := doc.NextHop.SubPort
		if port == 0 {
			port = DefaultSubPort
		}
		base = scheme + "://" + net.JoinHostPort(doc.NextHop.Host, strconv.Itoa(port))
	}
	if doc.NextHop.SubPath != "" {
		sub = doc.NextHop.SubPath
	}
	if doc.NextHop.JsonPath != "" {
		json = doc.NextHop.JsonPath
	}
	return base, sub, json
}

// route dispatches a request against the paths of the current document. While
// this hop has no document it knows neither its upstream nor its paths, so a
// subscription request is a 503 — a hop that answered 404 would look like a
// wrong link instead of a box still waiting for the wave (§5.4).
func (s *SubServer) route(c *gin.Context) {
	path := c.Request.URL.Path
	_, subPath, jsonPath := s.nextHop()
	doc := s.state.Document()

	if id, ok := subscriptionID(path, subPath, fallbackSubPath); ok {
		if doc == nil {
			c.String(http.StatusServiceUnavailable, "this box has not joined the chain yet")
			return
		}
		s.handleSub(c, id)
		return
	}
	if id, ok := subscriptionID(path, jsonPath, fallbackJsonPath); ok {
		if doc == nil {
			c.String(http.StatusServiceUnavailable, "this box has not joined the chain yet")
			return
		}
		s.handleJson(c, id)
		return
	}
	c.Status(http.StatusNotFound)
}

// subscriptionID matches a request path against the document's path and the
// built-in fallback, returning the subscription id.
func subscriptionID(path, documentPath, fallback string) (string, bool) {
	for _, prefix := range []string{documentPath, fallback} {
		if prefix == "" {
			continue
		}
		if id, found := strings.CutPrefix(path, prefix); found && id != "" && !strings.Contains(id, "/") {
			return id, true
		}
	}
	return "", false
}

// handleSub serves the raw subscription to apps and the custom page to browsers.
func (s *SubServer) handleSub(c *gin.Context, subid string) {
	base, subPath, _ := s.nextHop()
	body, header, status, err := s.fetchUpstream(base, subPath, subid)
	if err != nil || status != http.StatusOK || len(body) == 0 {
		logger.Warningf("proxy-front: next hop sub fetch failed (status %d): %v", status, err)
		c.String(http.StatusBadGateway, "subscription unavailable")
		return
	}
	if wantsHTML(c) {
		s.renderPage(c, subid, body, header)
		return
	}
	copyHeaders(c, header)
	c.Header("Profile-Web-Page-Url", s.publicURL(c, s.publicSubPath(), subid))
	c.String(http.StatusOK, string(body))
}

// handleJson proxies the JSON subscription straight through (the copy-JSON action).
func (s *SubServer) handleJson(c *gin.Context, subid string) {
	base, _, jsonPath := s.nextHop()
	body, header, status, err := s.fetchUpstream(base, jsonPath, subid)
	if err != nil || status != http.StatusOK || len(body) == 0 {
		c.String(http.StatusBadGateway, "subscription unavailable")
		return
	}
	copyHeaders(c, header)
	c.Header("Profile-Web-Page-Url", s.publicURL(c, s.publicSubPath(), subid))
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// publicSubPath is the path clients use on this hop — the same one it fetches
// under, because the chain forwards subscriptions one to one.
func (s *SubServer) publicSubPath() string {
	_, subPath, _ := s.nextHop()
	return subPath
}

// publicJsonPath is the JSON subscription path on this hop.
func (s *SubServer) publicJsonPath() string {
	_, _, jsonPath := s.nextHop()
	return jsonPath
}

// publicURL is the address of this hop's own subscription endpoint as a client
// outside sees it: the configured Domain, else the Host the request came in
// on. The panel builds its Profile-Web-Page-Url from the Host it was fetched
// by — an address deeper in the chain — so every hop must replace that header
// with its own identity, or subscription apps would carry a link inward.
func (s *SubServer) publicURL(c *gin.Context, path, subid string) string {
	scheme := s.cfg.Scheme()
	host := c.Request.Host
	if s.cfg.Domain != "" {
		host = PublicHostPort(scheme, s.cfg.Domain, s.cfg.SubPort)
	}
	return scheme + "://" + host + path + subid
}

// fetchUpstream GETs the raw subscription (not the HTML page) for the given
// path+id from the next hop.
func (s *SubServer) fetchUpstream(base, path, subid string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequest(http.MethodGet, base+path+subid, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.Header, resp.StatusCode, err
}

func copyHeaders(c *gin.Context, header http.Header) {
	for _, h := range passthroughHeaders {
		if v := header.Get(h); v != "" {
			c.Header(h, v)
		}
	}
}

func wantsHTML(c *gin.Context) bool {
	if c.Query("html") == "1" || strings.EqualFold(c.Query("view"), "html") {
		return true
	}
	return strings.Contains(strings.ToLower(c.GetHeader("Accept")), "text/html")
}

// pageData is the view model for subpage.html.
type pageData struct {
	Title   string
	SubURL  string
	JsonURL string
	QR      template.URL
	Configs []string
	Used    string
	Total   string
	Expire  string
	Apps    []app
}

func (s *SubServer) renderPage(c *gin.Context, subid string, body []byte, header http.Header) {
	subURL := s.publicURL(c, s.publicSubPath(), subid)
	jsonURL := s.publicURL(c, s.publicJsonPath(), subid)

	used, total, expire := parseUserinfo(header.Get("Subscription-Userinfo"))

	var qr template.URL
	if png, err := qrcode.Encode(subURL, qrcode.Medium, 256); err == nil {
		qr = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(c.Writer, pageData{
		Title:   "Subscription",
		SubURL:  subURL,
		JsonURL: jsonURL,
		QR:      qr,
		Configs: decodeConfigs(body),
		Used:    used,
		Total:   total,
		Expire:  expire,
		Apps:    recommendedApps,
	}); err != nil {
		logger.Warning("proxy-front: render page:", err)
	}
}

// decodeConfigs turns the raw subscription body (base64 or a plain newline list)
// into individual config links.
func decodeConfigs(body []byte) []string {
	raw := strings.TrimSpace(string(body))
	if dec, err := base64.StdEncoding.DecodeString(raw); err == nil {
		raw = string(dec)
	} else if dec, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		raw = string(dec)
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// parseUserinfo extracts human-readable used/total/expiry from a
// Subscription-Userinfo header ("upload=..; download=..; total=..; expire=..").
func parseUserinfo(h string) (used, total, expire string) {
	if h == "" {
		return "", "", ""
	}
	var up, down, tot, exp int64
	for _, part := range strings.Split(h, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		switch strings.TrimSpace(kv[0]) {
		case "upload":
			up = n
		case "download":
			down = n
		case "total":
			tot = n
		case "expire":
			exp = n
		}
	}
	used = common.FormatTraffic(up + down)
	total = "∞"
	if tot > 0 {
		total = common.FormatTraffic(tot)
	}
	if exp > 0 {
		expire = time.Unix(exp, 0).Format("2006-01-02")
	}
	return used, total, expire
}
