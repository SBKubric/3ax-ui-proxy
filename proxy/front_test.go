package proxy

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/nginx"
)

var updateFront = flag.Bool("update", false, "rewrite the golden front configs")

const (
	testIPCert = "/root/cert/ip/fullchain.pem"
	testIPKey  = "/root/cert/ip/privkey.pem"
)

// edgeFrontDocument is what the active edge of `real ← inner ← edge-a` holds: its
// own neighbour target, the inner as its next hop, and the relayed ports of the
// real server — 443 for the Reality clients, the AmneziaWG port and a TCP port
// of the operator's that only443 has no room for.
func edgeFrontDocument() *chain.Document {
	return &chain.Document{
		Version:  chain.DocumentVersion,
		Revision: 42,
		Self: chain.Self{Name: "edge-a", Role: chain.RoleEdge, Host: "198.51.100.20", State: chain.StateJoined,
			RealityTarget: "www.neighbour.example:443", RealityServerName: "www.neighbour.example"},
		NextHop: chain.NextHop{Host: "203.0.113.9", SubPort: 2096, SubScheme: "https",
			SubPath: "/sub-abc123/", JsonPath: "/json/", TunPath: "/tun/"},
		ActiveEdge: "edge-a",
		Hops: []chain.Hop{{Name: "edge-a", Role: chain.RoleEdge, Host: "198.51.100.20", SubPort: 2096,
			SecretHash: chain.HashSecret("edge-a-secret"), State: chain.StateJoined,
			RealityTarget: "www.neighbour.example:443", RealityServerName: "www.neighbour.example"}},
		Ports: []chain.Port{
			{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
			{Port: 8081, Network: chain.NetworkTCP, Tag: "extra-8081", Source: chain.SourceExtra},
			{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg},
		},
	}
}

// innerFrontDocument is the inner of the same chain: it relays for edge-a, the
// active edge, and for edge-b beside it.
func innerFrontDocument() *chain.Document {
	doc := edgeFrontDocument()
	doc.Self = chain.Self{Name: "inner-1", Role: chain.RoleInner, Host: "203.0.113.9", State: chain.StateJoined}
	doc.NextHop.Host = "192.0.2.1"
	doc.Hops = []chain.Hop{
		{Name: "inner-1", Role: chain.RoleInner, Host: "203.0.113.9", SubPort: 2096, SecretHash: chain.HashSecret("inner-1-secret"), State: chain.StateJoined},
		{Name: "edge-a", Role: chain.RoleEdge, Host: "198.51.100.20", SubPort: 2096, SecretHash: chain.HashSecret("edge-a-secret"), State: chain.StateJoined,
			RealityTarget: "www.neighbour.example:443", RealityServerName: "www.neighbour.example"},
		{Name: "edge-b", Role: chain.RoleEdge, Host: "198.51.100.30", SubPort: 2096, SecretHash: chain.HashSecret("edge-b-secret"), State: chain.StateJoined,
			RealityTarget: "cdn.other.example:443", RealityServerName: "cdn.other.example"},
	}
	return doc
}

func buildFrontT(t *testing.T, doc *chain.Document) (nginx.Config, []string) {
	t.Helper()
	cfg, warnings, err := BuildFront(doc, NewFrontLayout(doc, DefaultSubPort), testIPCert, testIPKey)
	if err != nil {
		t.Fatalf("BuildFront: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the front does not validate: %v", err)
	}
	return cfg, warnings
}

func renderFront(t *testing.T, cfg nginx.Config) (stream, http string) {
	t.Helper()
	stream, err := cfg.StreamConf()
	if err != nil {
		t.Fatal(err)
	}
	http, err = cfg.HTTPConf()
	if err != nil {
		t.Fatal(err)
	}
	return stream, http
}

// TestFrontOfAnEdge: the edge's own server name goes on raw to the next hop,
// an unknown one raw to its neighbour target, and a request by address to the
// HTTP side that fronts the box's sub server.
func TestFrontOfAnEdge(t *testing.T) {
	cfg, warnings := buildFrontT(t, edgeFrontDocument())
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	for _, route := range cfg.Routes {
		if !route.Raw {
			t.Errorf("route %q leaves the box with a PROXY header", route.Name)
		}
	}
	stream, http := renderFront(t, cfg)
	checkFrontGolden(t, "front_edge_stream.conf", stream)
	checkFrontGolden(t, "front_edge_http.conf", http)

	layout := NewFrontLayout(edgeFrontDocument(), DefaultSubPort)
	for _, want := range []string{
		"    www.neighbour.example " + layout.NextHopRelay + ";\n",
		"    \"\" " + layout.SiteListen + ";\n",
		"    default " + layout.TargetRelay + ";\n",
		// Between boxes the stream is raw: the relay takes the PROXY header
		// off before the next hop's front reads the SNI.
		"    listen " + layout.NextHopRelay + " proxy_protocol;\n    proxy_pass 203.0.113.9:443;\n",
		"    listen " + layout.TargetRelay + " proxy_protocol;\n    proxy_pass www.neighbour.example:443;\n",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream lacks %q", want)
		}
	}
	for _, want := range []string{"location /sub-abc123/ {", "location /json/ {", "location /chain/v1/ {", "location /join/ {",
		"proxy_pass http://" + layout.SubListen + ";", "ssl_certificate     " + testIPCert + ";"} {
		if !strings.Contains(http, want) {
			t.Errorf("HTTP side lacks %q", want)
		}
	}
}

// TestFrontOfAnEdgeWithoutATarget: no neighbour target in the document means
// nothing to hand an unknown SNI to but the decoy — and nothing to route the
// clients by either, which the owner has to hear about.
func TestFrontOfAnEdgeWithoutATarget(t *testing.T) {
	doc := edgeFrontDocument()
	doc.Self.RealityTarget, doc.Self.RealityServerName = "", ""
	cfg, warnings := buildFrontT(t, doc)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "neighbour target") {
		t.Errorf("warnings = %v, want one about the missing neighbour target", warnings)
	}
	stream, _ := renderFront(t, cfg)
	checkFrontGolden(t, "front_edge_notarget_stream.conf", stream)
	layout := NewFrontLayout(doc, DefaultSubPort)
	if !strings.Contains(stream, "    default "+layout.SiteListen+";\n") {
		t.Errorf("an unknown SNI does not get the decoy:\n%s", stream)
	}
}

// TestFrontOfAnInner: the active edge's server name goes on raw to the next
// hop; a standby edge's name, like any unknown one, gets the decoy. An inner has
// no Reality of its own to hide behind.
func TestFrontOfAnInner(t *testing.T) {
	doc := innerFrontDocument()
	cfg, warnings := buildFrontT(t, doc)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	stream, http := renderFront(t, cfg)
	checkFrontGolden(t, "front_inner_stream.conf", stream)
	checkFrontGolden(t, "front_inner_http.conf", http)
	layout := NewFrontLayout(doc, DefaultSubPort)
	if !strings.Contains(stream, "    www.neighbour.example "+layout.NextHopRelay+";\n") {
		t.Error("the active edge's server name is not passed on")
	}
	if strings.Contains(stream, "cdn.other.example") {
		t.Error("a standby edge's server name is routed; only the active edge's is")
	}
	if !strings.Contains(stream, "    default "+layout.SiteListen+";\n") {
		t.Error("an unknown SNI on an inner does not get the decoy")
	}
	if !strings.Contains(stream, "proxy_pass 192.0.2.1:443;") {
		t.Error("the raw stream does not go to the next hop's 443")
	}
}

// TestFrontOfAnInnerWithoutAnActiveEdge: nothing to pass on, so everything but
// the HTTP side is the decoy — and the log says why the clients are not served.
func TestFrontOfAnInnerWithoutAnActiveEdge(t *testing.T) {
	doc := innerFrontDocument()
	doc.ActiveEdge = ""
	cfg, warnings := buildFrontT(t, doc)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "active edge") {
		t.Errorf("warnings = %v, want one about the active edge", warnings)
	}
	if len(cfg.Routes) != 0 {
		t.Errorf("routes = %+v, want none", cfg.Routes)
	}
}

// TestFrontNeedsTheIPCertificate: the HTTP side is the only way to the box's
// sub server once it moves to the loopback, and by address it needs the IP
// certificate. Without one the front must not come up at all.
func TestFrontNeedsTheIPCertificate(t *testing.T) {
	doc := edgeFrontDocument()
	if _, _, err := BuildFront(doc, NewFrontLayout(doc, DefaultSubPort), "", ""); err == nil {
		t.Error("a front without a certificate was built")
	}
}

// TestFrontLayoutAvoidsTheRelayedPorts: the loopback ports the front picks
// must not be ports the relay binds on every address, nor the old sub port —
// and the same document must give the same ports, or every poll would reload
// nginx.
func TestFrontLayoutAvoidsTheRelayedPorts(t *testing.T) {
	doc := edgeFrontDocument() // relays 8081
	layout := NewFrontLayout(doc, 8082)
	used := map[string]bool{}
	for _, addr := range []string{layout.SiteListen, layout.SubListen, layout.NextHopRelay, layout.TargetRelay} {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("%q is not on the loopback", addr)
		}
		if strings.HasSuffix(addr, ":8081") || strings.HasSuffix(addr, ":8082") {
			t.Errorf("%q collides with a relayed port or the sub port", addr)
		}
		if used[addr] {
			t.Errorf("%q is used twice", addr)
		}
		used[addr] = true
	}
	if again := NewFrontLayout(edgeFrontDocument(), 8082); again != layout {
		t.Errorf("the layout wanders: %+v then %+v", layout, again)
	}
}

func checkFrontGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateFront {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden (regenerate with go test ./proxy/ -run TestFront -update):\n%s", name, got)
	}
}

// TestRelayPortsBehindTheFront: with the front up, nginx owns 443/tcp, so the
// relay must not bind it; UDP is relayed as before, 443/udp included; and a TCP
// port other than 443 has no way in any more, so it is not relayed and the owner
// is told which.
func TestRelayPortsBehindTheFront(t *testing.T) {
	ports := []chain.Port{
		{Port: 443, Network: chain.NetworkTCPUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8443, Network: chain.NetworkTCPUDP, Tag: "inbound-trojan", Source: chain.SourceXray},
		{Port: 9443, Network: chain.NetworkTCP, Tag: "mtproto-3", Source: chain.SourceMtproto},
		{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg},
	}

	kept, dropped := RelayPorts(ports, true)
	want := []chain.Port{
		{Port: 443, Network: chain.NetworkUDP, Tag: "inbound-443", Source: chain.SourceXray},
		{Port: 8443, Network: chain.NetworkUDP, Tag: "inbound-trojan", Source: chain.SourceXray},
		{Port: 51820, Network: chain.NetworkUDP, Tag: "awg", Source: chain.SourceAwg},
	}
	if len(kept) != len(want) {
		t.Fatalf("kept = %+v, want %+v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("kept[%d] = %+v, want %+v", i, kept[i], want[i])
		}
	}
	if len(dropped) != 2 || dropped[0] != 8443 || dropped[1] != 9443 {
		t.Errorf("dropped = %v, want [8443 9443] — 443/tcp is nginx's, not a loss", dropped)
	}

	// With the front off nothing changes.
	kept, dropped = RelayPorts(ports, false)
	if len(kept) != len(ports) || len(dropped) != 0 {
		t.Errorf("front off: kept %+v dropped %v", kept, dropped)
	}
}

// TestFrontFirewallFromTheDocument: 443 for the front, 80 for the ACME
// webroot, the relayed UDP ports, and the old sub port only while an outer
// neighbour may still be polling it. SSH is ApplyFirewall's own business.
func TestFrontFirewallFromTheDocument(t *testing.T) {
	kept, _ := RelayPorts(edgeFrontDocument().Ports, true)

	fw := FrontFirewall(kept, 0)
	if got := portList(fw.TCP); got != "443,80" {
		t.Errorf("TCP = %s, want 443,80", got)
	}
	if got := portList(fw.UDP); got != "443,51820" {
		t.Errorf("UDP = %s, want the relayed UDP ports 443,51820", got)
	}

	fw = FrontFirewall(kept, 2096)
	if got := portList(fw.TCP); got != "443,80,2096" {
		t.Errorf("TCP during the move = %s, want the old sub port kept open", got)
	}
}

func portList(ranges []nginx.PortRange) string {
	var parts []string
	for _, r := range ranges {
		parts = append(parts, strconv.Itoa(r.From))
	}
	return strings.Join(parts, ",")
}

// TestOldSubPortStaysUntilTheOuterNeighboursMoved is the transition rule of
// #140. The front comes up at revision `since`; the panel hears of it on the
// next poll and bumps the revision, and every document this box hands out from
// then on points its outer neighbours at 443. A neighbour that acknowledges a
// revision newer than `since` has applied such a document and dials 443 now.
// Until every direct outer neighbour has, the old sub port keeps answering —
// otherwise the one poll that carries the news would find the door shut.
func TestOldSubPortStaysUntilTheOuterNeighboursMoved(t *testing.T) {
	const since = 42
	ack := func(name string, revision int64) OuterAck {
		return OuterAck{Name: name, LastRevision: revision, LastSeen: 1}
	}
	inner := innerFrontDocument() // hops: inner-1 (self), edge-a, edge-b

	cases := []struct {
		name string
		doc  *chain.Document
		acks []OuterAck
		want bool
	}{
		{name: "an edge has nobody polling it", doc: edgeFrontDocument(), want: false},
		{name: "no neighbour has polled yet", doc: inner, want: true},
		{name: "one edge moved, the other not yet", doc: inner, acks: []OuterAck{ack("edge-a", 43), ack("edge-b", 42)}, want: true},
		{name: "both edges moved", doc: inner, acks: []OuterAck{ack("edge-a", 43), ack("edge-b", 44)}, want: false},
		{
			// Edges hang off the last inner: here that is inner-2, the one
			// neighbour of inner-1 whose move counts.
			name: "only the next inner out counts",
			doc: withHops(inner, chain.Hop{Name: "inner-1", Role: chain.RoleInner, State: chain.StateJoined},
				chain.Hop{Name: "inner-2", Role: chain.RoleInner, State: chain.StateJoined},
				chain.Hop{Name: "edge-a", Role: chain.RoleEdge, State: chain.StateJoined}),
			acks: []OuterAck{ack("inner-2", 43)},
			want: false,
		},
		{
			// A draining inner still polls this box until its neighbours
			// have re-chained past it (§4.5), and so does the inner beyond.
			name: "a draining inner in between polls too",
			doc: withHops(inner, chain.Hop{Name: "inner-1", Role: chain.RoleInner, State: chain.StateJoined},
				chain.Hop{Name: "inner-2", Role: chain.RoleInner, State: chain.StateDraining},
				chain.Hop{Name: "inner-3", Role: chain.RoleInner, State: chain.StateJoined},
				chain.Hop{Name: "edge-a", Role: chain.RoleEdge, State: chain.StateJoined}),
			acks: []OuterAck{ack("inner-3", 43)},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OldSubPortNeeded(tc.doc, since, tc.acks); got != tc.want {
				t.Errorf("OldSubPortNeeded = %t, want %t", got, tc.want)
			}
		})
	}
}

func withHops(doc *chain.Document, hops ...chain.Hop) *chain.Document {
	copied := *doc
	copied.Hops = hops
	return &copied
}
