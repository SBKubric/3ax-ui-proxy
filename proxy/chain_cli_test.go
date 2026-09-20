package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// TestRejoinPointsTheBoxAtANewNextHop is how a runbook repairs a chain whose
// inner hop died (§4.6): a fresh token, a new next hop, no page and no
// waiting for a restart to write the result down.
func TestRejoinPointsTheBoxAtANewNextHop(t *testing.T) {
	var got joinRequest
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(joinAnswer())
	}))
	defer panel.Close()
	host, port := hostPort(t, panel.URL)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":2,"nextHop":{"host":"the-dead-inner"},"hopSecret":"the-old-secret","domain":"b.example.net"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = filepath.Join(dir, "chain")

	result, err := Rejoin(context.Background(), cfg, host, port, "http", "0123456789012345678901234567890a")
	if err != nil {
		t.Fatalf("Rejoin: %v", err)
	}
	if result.Document.Self.Name != "edge-b" {
		t.Errorf("result = %+v", result)
	}
	if got.Token != "0123456789012345678901234567890a" {
		t.Errorf("the token did not reach the chain: %+v", got)
	}

	saved, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.NextHop.Host != host || saved.NextHop.SubPort != port || saved.NextHop.SubScheme != "http" {
		t.Errorf("saved nextHop = %+v, want the new one", saved.NextHop)
	}
	if saved.HopSecret != "the-issued-hop-secret" {
		t.Errorf("saved hopSecret = %q, want the freshly issued one", saved.HopSecret)
	}
	cached, err := NewDocumentStore(saved.DocumentPath()).Load()
	if err != nil || cached == nil || cached.Revision != 43 {
		t.Fatalf("the document was not cached: %+v (%v)", cached, err)
	}
}

// TestRejoinNeedsATokenAndANextHop: rejoin exists precisely because the old
// secret is dead, so guessing either argument would only produce a confusing
// 404 from deep inside the chain (§5.8 — there is no --yes either).
func TestRejoinNeedsATokenAndANextHop(t *testing.T) {
	cfg := &Config{NextHop: NextHop{Host: "x", SubPort: 2096, SubScheme: "https"}, HopSecret: "old"}
	if _, err := Rejoin(context.Background(), cfg, "", 0, "", "token"); err == nil {
		t.Error("rejoin without --next-hop was accepted")
	}
	if _, err := Rejoin(context.Background(), cfg, "10.0.0.7", 0, "", "  "); err == nil {
		t.Error("rejoin without --token was accepted")
	}
	if cfg.HopSecret != "old" {
		t.Error("a refused rejoin threw the running secret away")
	}
}

// TestFetchAndPrintStatus: `x-ui chain status` asks the running process on the
// loopback with the box's own secret, because the truth about a hop lives in
// the process, not in its config file.
func TestFetchAndPrintStatus(t *testing.T) {
	state := NewState()
	state.SetDocument(innerDocument())
	state.MarkPoll(1758379990000, true)

	cfg := &Config{HopSecret: "inner-1-secret", Domain: "10.0.0.7"}
	server := httptest.NewServer(testChainHandler(t, cfg, state))
	defer server.Close()
	host, port := hostPort(t, server.URL)
	if host != "127.0.0.1" {
		t.Skipf("the test server is not on the loopback (%s)", host)
	}
	cfg.SubPort = port

	status, err := FetchStatus(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if status.Name != "inner-1" || status.Revision != 42 {
		t.Errorf("status = %+v", status)
	}

	var out bytes.Buffer
	status.Relay = chain.StatusRelay{Running: true, Ports: []int{443, 51820}}
	PrintStatus(&out, status)
	printed := out.String()
	for _, want := range []string{"inner-1", "inner", "198.51.100.1:" + strconv.Itoa(status.NextHop.SubPort), "443 51820", "42"} {
		if !strings.Contains(printed, want) {
			t.Errorf("printed status lacks %q:\n%s", want, printed)
		}
	}

	// A box that has not joined has nothing to ask.
	if _, err := FetchStatus(context.Background(), &Config{SubPort: port}); err == nil {
		t.Error("a bootstrap box reported a status")
	}
}
