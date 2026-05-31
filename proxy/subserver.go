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

// headers copied through from the real panel to subscription clients.
var passthroughHeaders = []string{
	"Subscription-Userinfo", "Profile-Update-Interval", "Profile-Title",
	"Profile-Web-Page-Url", "Support-Url", "Announce", "Routing-Enable", "Routing",
}

// SubServer is the proxy's subscription endpoint. It serves /sub and /json by
// fetching them from the real panel (UpstreamBase) and, for browsers, renders a
// custom info page with recommended apps and a copy-VLESS-JSON action.
type SubServer struct {
	cfg        *Config
	tmpl       *template.Template
	client     *http.Client
	httpServer *http.Server
}

// NewSubServer builds the proxy subscription server (does not start it).
func NewSubServer(cfg *Config) (*SubServer, error) {
	tmpl, err := template.New("subpage").Parse(subpageHTML)
	if err != nil {
		return nil, fmt.Errorf("parse proxy subpage template: %w", err)
	}
	return &SubServer{
		cfg:  cfg,
		tmpl: tmpl,
		client: &http.Client{
			Timeout: 15 * time.Second,
			// The proxy reaches the real panel by its hidden address (often a bare
			// IP and/or a self-signed cert), so TLS verification is skipped for this
			// server-to-server hop; trust is established out of band.
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}, nil
}

// Start binds and serves the subscription endpoint in a background goroutine.
func (s *SubServer) Start() error {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.GET(s.cfg.SubPath+":subid", s.handleSub)
	engine.GET(s.cfg.JsonPath+":subid", s.handleJson)

	addr := net.JoinHostPort(s.cfg.SubListen, strconv.Itoa(s.cfg.SubPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy sub server listen %s: %w", addr, err)
	}
	s.httpServer = &http.Server{Handler: engine}

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

	logger.Infof("proxy-front: subscription server on %s%s (upstream %s)", addr, s.cfg.SubPath, s.cfg.UpstreamBase)
	return nil
}

// Stop shuts the subscription server down.
func (s *SubServer) Stop() error {
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

// handleSub serves the raw subscription to apps and the custom page to browsers.
func (s *SubServer) handleSub(c *gin.Context) {
	body, header, status, err := s.fetchUpstream(s.cfg.SubPath, c.Param("subid"))
	if err != nil || status != http.StatusOK || len(body) == 0 {
		logger.Warningf("proxy-front: upstream sub fetch failed (status %d): %v", status, err)
		c.String(http.StatusBadGateway, "subscription unavailable")
		return
	}
	if wantsHTML(c) {
		s.renderPage(c, c.Param("subid"), body, header)
		return
	}
	copyHeaders(c, header)
	c.String(http.StatusOK, string(body))
}

// handleJson proxies the JSON subscription straight through (the copy-JSON action).
func (s *SubServer) handleJson(c *gin.Context) {
	body, header, status, err := s.fetchUpstream(s.cfg.JsonPath, c.Param("subid"))
	if err != nil || status != http.StatusOK || len(body) == 0 {
		c.String(http.StatusBadGateway, "subscription unavailable")
		return
	}
	copyHeaders(c, header)
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// fetchUpstream GETs the raw subscription (not the panel's HTML page) for the
// given path+id from the real panel.
func (s *SubServer) fetchUpstream(path, subid string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequest(http.MethodGet, s.cfg.UpstreamBase+path+subid, nil)
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
	scheme := "http"
	if s.cfg.TLS() {
		scheme = "https"
	}
	host := s.cfg.Domain
	if host == "" {
		host = c.Request.Host
	}
	subURL := scheme + "://" + host + s.cfg.SubPath + subid
	jsonURL := scheme + "://" + host + s.cfg.JsonPath + subid

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
