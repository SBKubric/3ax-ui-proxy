package nginx

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden config files")

// goldenConfig is the shape of a real server: a Reality inbound with two cover
// names that also takes the unmatched connections, and an MTProto inbound
// behind its own FakeTLS domain.
func goldenConfig() Config {
	return Config{
		Mode: ModeShared,
		Port: 443,
		Routes: []Route{
			{
				Name:     "ru-vpn (VLESS Reality)",
				SNIs:     []string{"www.icloud.com", "apple.com"},
				Upstream: "127.0.0.1:8443",
				Fallback: true,
			},
			{
				// Keeps its own port, so nginx has to take the PROXY header
				// off again before handing the stream over — the same
				// listener also serves people connecting to 4343 directly.
				Name:     "Telegram-via-nl (MTProto)",
				SNIs:     []string{"www.samsung.com"},
				Upstream: "127.0.0.1:4343",
				Relay:    "127.0.0.1:8081",
			},
		},
	}
}

func goldenSite() *Site {
	return &Site{
		Domain:   "net-ru.modulator.net",
		CertFile: "/root/cert/net-ru.modulator.net/fullchain.pem",
		KeyFile:  "/root/cert/net-ru.modulator.net/privkey.pem",
		Listen:   "127.0.0.1:8080",
		Root:     "/usr/local/x-ui/www",
	}
}

func TestGeneratedConfigs(t *testing.T) {
	// Passthrough only: no domain yet, so 443 is pure SNI routing.
	c := goldenConfig()
	check(t, "stream_passthrough.conf", must(t, c.StreamConf))
	if got := must(t, c.HTTPConf); got != "" {
		t.Errorf("a config without a site should produce no http block, got:\n%s", got)
	}

	// Shared mode with the stub site on our own domain.
	c.Site = goldenSite()
	check(t, "stream_shared.conf", must(t, c.StreamConf))
	check(t, "http_shared.conf", must(t, c.HTTPConf))

	// Everything behind 443: panel and subscriptions proxied too.
	c.Mode = ModeOnly443
	c.Site.Panel = &Proxy{
		Name: "panel", Paths: []string{"/uS2J19TzcfZuEAPyNH/"},
		Target: "127.0.0.1:33757", TLS: true,
	}
	c.Site.Sub = &Proxy{
		Name: "subscriptions", Paths: []string{"/sub-abc123/", "/json/"},
		Target: "127.0.0.1:2096",
	}
	check(t, "stream_only443.conf", must(t, c.StreamConf))
	check(t, "http_only443.conf", must(t, c.HTTPConf))
}

// goldenIPCert is the Let's Encrypt IP certificate install.sh leaves on every
// box: what the HTTP side answers with when a client asks by address, with no
// SNI to route on.
func goldenIPCert(s *Site) {
	s.IPCertFile = "/root/cert/ip/fullchain.pem"
	s.IPKeyFile = "/root/cert/ip/privkey.pem"
}

func TestGeneratedConfigsWithIPCertificate(t *testing.T) {
	// Only the IP certificate: a box with no domain of its own still
	// terminates requests by address.
	c := goldenConfig()
	c.Site = goldenSite()
	c.Site.Domain, c.Site.CertFile, c.Site.KeyFile = "", "", ""
	goldenIPCert(c.Site)
	check(t, "stream_ip.conf", must(t, c.StreamConf))
	check(t, "http_ip.conf", must(t, c.HTTPConf))

	// Both: the domain keeps its own block and certificate, the address gets
	// the default server with the IP certificate and the same locations.
	c = goldenConfig()
	c.Mode = ModeOnly443
	c.Site = goldenSite()
	goldenIPCert(c.Site)
	c.Site.Panel = &Proxy{
		Name: "panel", Paths: []string{"/uS2J19TzcfZuEAPyNH/"},
		Target: "127.0.0.1:33757", TLS: true,
	}
	c.Site.Sub = &Proxy{
		Name: "subscriptions", Paths: []string{"/sub-abc123/", "/json/"},
		Target: "127.0.0.1:2096",
	}
	stream := must(t, c.StreamConf)
	http := must(t, c.HTTPConf)
	check(t, "stream_only443_ip.conf", stream)
	check(t, "http_only443_ip.conf", http)

	// What the goldens have to keep saying, spelled out.
	if !strings.Contains(stream, "    \"\" 127.0.0.1:8080;\n") {
		t.Error("an empty SNI is not sent to the HTTP side")
	}
	if !strings.Contains(stream, "    default 127.0.0.1:8443;\n") {
		t.Error("an unknown SNI no longer goes to the Reality fallback")
	}
	if n := strings.Count(http, "default_server"); n != 1 {
		t.Fatalf("%d default_server listens, want exactly one", n)
	}
	_, ipBlock, _ := strings.Cut(http, "default_server")
	if !strings.Contains(ipBlock, "server_name _;") || !strings.Contains(ipBlock, "ssl_certificate     /root/cert/ip/fullchain.pem;") {
		t.Error("the default server does not carry the IP certificate")
	}
	if strings.Count(http, "location /uS2J19TzcfZuEAPyNH/ {") != 2 || strings.Count(http, "location /sub-abc123/ {") != 2 {
		t.Error("the address does not serve the same paths as the domain")
	}
}

// goldenHopConfig is the front of a box in the proxy chain (#140): the edge's
// server name leaves raw for the next hop's 443, an unknown one raw for the
// neighbour target, and a request by address lands on the HTTP side.
func goldenHopConfig() Config {
	return Config{
		Mode: ModeOnly443,
		Port: 443,
		Routes: []Route{
			{Name: "clients of edge-a → next hop", SNIs: []string{"www.neighbour.example"},
				Upstream: "203.0.113.9:443", Relay: "127.0.0.1:8084", Raw: true},
			{Name: "neighbour target of edge-a", Upstream: "www.neighbour.example:443",
				Relay: "127.0.0.1:8085", Raw: true, Fallback: true},
		},
		Site: &Site{
			IPCertFile: "/root/cert/ip/fullchain.pem",
			IPKeyFile:  "/root/cert/ip/privkey.pem",
			Listen:     "127.0.0.1:8082",
			Root:       "/usr/local/x-ui/www",
			Sub:        &Proxy{Name: "sub server of edge-a", Paths: []string{"/sub/", "/chain/v1/"}, Target: "127.0.0.1:8083"},
		},
	}
}

// TestGeneratedHopConfig: between boxes the stream carries no PROXY header.
// The next hop's front reads the SNI from the very first bytes it gets, and a
// neighbour's site has never heard of the header, so a raw route has it taken
// off on the loopback before the stream leaves — while the HTTP side on the
// same box keeps it, and with it the client's address.
func TestGeneratedHopConfig(t *testing.T) {
	c := goldenHopConfig()
	stream := must(t, c.StreamConf)
	check(t, "stream_hop_raw.conf", stream)
	for _, want := range []string{
		"    listen 127.0.0.1:8084 proxy_protocol;\n    proxy_pass 203.0.113.9:443;\n",
		"    default 127.0.0.1:8085;\n",
		"# clients of edge-a → next hop — raw stream: the PROXY header is taken off before it leaves the box\n",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream lacks %q", want)
		}
	}
	if strings.Contains(stream, "keeps its own port") {
		t.Error("a raw route is described as an inbound that kept its own port")
	}
	check(t, "http_hop.conf", must(t, c.HTTPConf))
}

// TestHTTPSideServesMonitoring: the monitoring contract lives under the
// panel's base path on the panel's own port, which only443 closes. mon-server
// reaches it through the HTTP side instead (ADR 0005), whether or not the rest
// of the panel is published there.
func TestHTTPSideServesMonitoring(t *testing.T) {
	c := goldenConfig()
	c.Mode = ModeOnly443
	c.Site = goldenSite()
	c.Site.Mon = &Proxy{Name: "monitoring", Paths: []string{"/uS2J19TzcfZuEAPyNH/mon/v1/"}, Target: "127.0.0.1:33757", TLS: true}
	http := must(t, c.HTTPConf)
	for _, want := range []string{
		"    # monitoring\n    location /uS2J19TzcfZuEAPyNH/mon/v1/ {\n        proxy_pass https://127.0.0.1:33757;\n",
		// The websocket map is emitted for any proxied service.
		"map $http_upgrade $threeax_connection_upgrade {",
	} {
		if !strings.Contains(http, want) {
			t.Errorf("the HTTP side lacks %q:\n%s", want, http)
		}
	}

	c.Site.Mon.Target = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "monitoring has no target") {
		t.Errorf("a monitoring proxy without a target validated: %v", err)
	}
}

// TestFallbackPrefersReality guards the reasoning behind the default entry: a
// prober with an unknown server name has to land on Reality, which answers it
// by proxying to the site it borrows its identity from. Sending it to our own
// certificate instead would answer a question nobody asked.
func TestFallbackPrefersReality(t *testing.T) {
	c := goldenConfig()
	c.Site = goldenSite()
	if got := c.fallbackUpstream(); got != "127.0.0.1:8443" {
		t.Errorf("fallback = %q, want the Reality upstream", got)
	}

	// Without a Reality inbound there is nothing to hide behind, so the site
	// takes over.
	c.Routes = c.Routes[1:]
	if got := c.fallbackUpstream(); got != "127.0.0.1:8080" {
		t.Errorf("fallback without Reality = %q, want the site", got)
	}
}

// TestFreeLoopbackPortAvoidsWhatIsTaken guards against the constant this used
// to be. A hard-coded 8080 fails on any server that already has something
// there — and the failure is silent: nginx keeps the old config, and the stream
// block forwards our own domain to whatever does hold the port.
func TestFreeLoopbackPortAvoidsWhatIsTaken(t *testing.T) {
	// Take whichever port in the scanned range happens to be free, so the test
	// does not depend on what else this machine is running.
	free, err := FreeLoopbackPort(0, nil)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	occupied, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(free)))
	if err != nil {
		t.Skipf("could not take port %d for the test: %v", free, err)
	}
	defer occupied.Close()
	taken := free

	got, err := FreeLoopbackPort(taken, nil)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got == taken {
		t.Errorf("picked %d, which is already in use", got)
	}
	if got < 8081 || got > 8999 {
		t.Errorf("picked %d, outside the range that avoids the ephemeral ports", got)
	}

	// A port that is genuinely free must be kept: the panel stores its choice
	// and must not wander to a new port on every reconcile.
	if again, err := FreeLoopbackPort(got, nil); err != nil || again != got {
		t.Errorf("a free port was not kept: %d -> %d (%v)", got, again, err)
	}
}

func TestModeOffGeneratesNothing(t *testing.T) {
	c := goldenConfig()
	c.Mode = ModeOff
	c.Site = goldenSite()
	if got := must(t, c.StreamConf); got != "" {
		t.Errorf("mode off produced a stream config:\n%s", got)
	}
	if got := must(t, c.HTTPConf); got != "" {
		t.Errorf("mode off produced an http config:\n%s", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		want string // substring of the expected error, "" for valid
		mut  func(c *Config)
	}{
		{name: "valid passthrough"},
		{name: "valid with site", mut: func(c *Config) { c.Site = goldenSite() }},

		{
			// The one mistake nginx cannot catch: the map keeps a single
			// entry per name and the loser goes quietly dark.
			name: "two inbounds share a cover name",
			want: "claimed by both",
			mut:  func(c *Config) { c.Routes[1].SNIs = []string{"APPLE.com"} },
		},
		{
			name: "our domain collides with a cover name",
			want: "claimed by both",
			mut: func(c *Config) {
				c.Site = goldenSite()
				c.Routes[1].SNIs = []string{c.Site.Domain}
			},
		},
		{
			name: "unknown mode",
			want: "unknown nginx mode",
			mut:  func(c *Config) { c.Mode = "maybe" },
		},
		{
			name: "port out of range",
			want: "out of range",
			mut:  func(c *Config) { c.Port = 70000 },
		},
		{
			name: "route without an upstream",
			want: "no upstream",
			mut:  func(c *Config) { c.Routes[1].Upstream = "" },
		},
		{
			name: "relay pointing at its own upstream",
			want: "relays to itself",
			mut:  func(c *Config) { c.Routes[1].Relay = c.Routes[1].Upstream },
		},
		{
			// Two relays on one address means one of the two inbounds silently
			// receives the other's traffic.
			name: "two routes share a relay",
			want: "share the relay address",
			mut: func(c *Config) {
				c.Routes[0].Relay = "127.0.0.1:8081"
				c.Routes[1].Relay = "127.0.0.1:8081"
			},
		},
		{
			name: "route that matches nothing",
			want: "matches no server name",
			mut:  func(c *Config) { c.Routes[1].SNIs = nil },
		},
		{
			name: "empty server name",
			want: "empty server name",
			mut:  func(c *Config) { c.Routes[1].SNIs = []string{"  "} },
		},
		{
			name: "two fallbacks",
			want: "more than one route claims the fallback",
			mut:  func(c *Config) { c.Routes[1].Fallback = true },
		},
		{
			name: "site without a certificate",
			want: "no certificate",
			mut: func(c *Config) {
				c.Site = goldenSite()
				c.Site.CertFile = ""
			},
		},
		{
			name: "site without a domain",
			want: "no domain",
			mut: func(c *Config) {
				c.Site = goldenSite()
				c.Site.Domain = ""
			},
		},
		{
			// A box with no domain of its own serves by address alone.
			name: "site with only an IP certificate",
			mut: func(c *Config) {
				c.Site = goldenSite()
				c.Site.Domain, c.Site.CertFile, c.Site.KeyFile = "", "", ""
				goldenIPCert(c.Site)
			},
		},
		{
			name: "site with both a domain and an IP certificate",
			mut: func(c *Config) {
				c.Site = goldenSite()
				goldenIPCert(c.Site)
			},
		},
		{
			name: "IP certificate without its key",
			want: "no key for the IP certificate",
			mut: func(c *Config) {
				c.Site = goldenSite()
				goldenIPCert(c.Site)
				c.Site.IPKeyFile = ""
			},
		},
		{
			name: "a domain without its certificate next to an IP certificate",
			want: "no certificate",
			mut: func(c *Config) {
				c.Site = goldenSite()
				goldenIPCert(c.Site)
				c.Site.CertFile = ""
			},
		},
		{
			name: "proxy without a path",
			want: "has no path",
			mut: func(c *Config) {
				c.Site = goldenSite()
				c.Site.Panel = &Proxy{Name: "panel", Target: "127.0.0.1:33757"}
			},
		},
		{
			name: "nothing to serve",
			want: "nothing to serve",
			mut:  func(c *Config) { c.Routes = nil },
		},
		{
			// A raw route without the loopback listener that takes the PROXY
			// header off would hand the header to another box's front.
			name: "raw route without a relay",
			want: "raw route",
			mut:  func(c *Config) { c.Routes[1].Relay = ""; c.Routes[1].Raw = true },
		},
		{
			// mode off is never wrong, whatever else is in the struct.
			name: "mode off ignores the rest",
			mut: func(c *Config) {
				c.Mode = ModeOff
				c.Port = 0
				c.Routes = nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goldenConfig()
			if tc.mut != nil {
				tc.mut(&c)
			}
			err := c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("expected an error containing %q, got none", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestStreamConfIsStable makes sure a save that changed nothing renders the
// same bytes: the service compares the rendered config with what is on disk to
// decide whether to reload, and a map that iterated in random order would
// reload nginx on every save.
func TestStreamConfIsStable(t *testing.T) {
	c := goldenConfig()
	c.Routes[0].SNIs = []string{"apple.com", "WWW.iCloud.com  "}
	first := must(t, c.StreamConf)
	c.Routes[0].SNIs = []string{"  www.icloud.com", "APPLE.COM"}
	if second := must(t, c.StreamConf); first != second {
		t.Errorf("the same names in another order rendered differently:\n%s\n---\n%s", first, second)
	}
}

func must(t *testing.T, fn func() (string, error)) string {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return got
}

func check(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if got == string(want) {
		return
	}
	t.Errorf("%s differs from the golden", name)
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		g, w := at(gotLines, i), at(wantLines, i)
		if g != w {
			t.Errorf("  строка %d:\n    эталон: %s\n    вывод:  %s", i+1, w, g)
		}
	}
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "(нет строки)"
}
