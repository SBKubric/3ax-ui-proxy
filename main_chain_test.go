package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/proxy"
)

// `x-ui chain …` is how an operator drives a box by hand and how install.sh
// drives one without a browser (docs/spec/proxy-chain.md §5.8). These tests
// cover the argument handling only: what each subcommand then does lives in
// package proxy, where it can be exercised against a fake chain.

func runChain(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := chainCommand(args, &out)
	return code, out.String()
}

func TestChainCommandNeedsASubcommand(t *testing.T) {
	code, out := runChain(t)
	if code == 0 {
		t.Error("bare `x-ui chain` succeeded")
	}
	for _, want := range []string{"ports", "join-url", "status", "rejoin"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage line does not name %q: %q", want, out)
		}
	}
}

// The old names are gone without an alias (§5.8): `proxy-setup-url` and
// `relay-manifest` lived in one scenario, and a box from that scenario does
// not reach a chain without being reinstalled.
func TestChainCommandRejectsTheRetiredNames(t *testing.T) {
	for _, name := range []string{"setup-url", "relay-manifest", "manifest"} {
		code, out := runChain(t, name)
		if code == 0 {
			t.Errorf("`x-ui chain %s` succeeded", name)
		}
		if !strings.Contains(out, "unknown subcommand") {
			t.Errorf("`x-ui chain %s` said %q", name, out)
		}
	}
}

func TestChainJoinURLPrintsThePendingLink(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "proxy.json")

	code, out := runChain(t, "join-url", "-c", cfgPath)
	if code == 0 {
		t.Error("a box with no pending join page reported a URL")
	}
	if !strings.Contains(out, "already joined") {
		t.Errorf("join-url said %q", out)
	}

	url := "https://b.example.net:2096/join/abc"
	if err := os.WriteFile(proxy.JoinURLPath(cfgPath), []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = runChain(t, "join-url", "-c", cfgPath)
	if code != 0 || strings.TrimSpace(out) != url {
		t.Errorf("join-url = %d %q, want 0 and %q", code, out, url)
	}
}

func TestChainStatusAndRejoinRefuseAMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "proxy.json")

	code, out := runChain(t, "status", "-c", missing)
	if code == 0 || !strings.Contains(out, "proxy.json") {
		t.Errorf("status on a missing config = %d %q", code, out)
	}

	code, out = runChain(t, "rejoin", "-c", missing, "--next-hop", "10.0.0.7", "--token", "x")
	if code == 0 || !strings.Contains(out, "proxy.json") {
		t.Errorf("rejoin on a missing config = %d %q", code, out)
	}
}

// A rejoin without a token is refused before anything is written: the whole
// point of the command is that the old secret is dead and only a freshly
// issued token can replace it (§4.6).
func TestChainRejoinNeedsAFreshToken(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "proxy.json")
	if err := os.WriteFile(cfgPath, []byte(`{"version":2,"nextHop":{"host":"10.0.0.7"},"hopSecret":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runChain(t, "rejoin", "-c", cfgPath, "--next-hop", "10.0.0.9")
	if code == 0 || !strings.Contains(out, "--token") {
		t.Errorf("rejoin without --token = %d %q", code, out)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"hopSecret": "old"`) && !strings.Contains(string(saved), `"hopSecret":"old"`) {
		t.Errorf("the config was rewritten by a refused rejoin: %s", saved)
	}
}
