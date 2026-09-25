package main

import (
	"bytes"
	"strings"
	"testing"
)

// `x-ui nginx acme-front` is what install.sh, update.sh and x-ui.sh run before
// every certificate issuance. What it does lives in package nginx; these
// tests cover the command line only.

func runNginx(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := nginxCommand(args, &out)
	return code, out.String()
}

func TestNginxCommandNeedsASubcommand(t *testing.T) {
	code, out := runNginx(t)
	if code == 0 {
		t.Error("bare `x-ui nginx` succeeded")
	}
	if !strings.Contains(out, "acme-front") {
		t.Errorf("the usage line does not name acme-front: %q", out)
	}
}

func TestNginxCommandRejectsAnUnknownSubcommand(t *testing.T) {
	code, out := runNginx(t, "acme-back")
	if code == 0 || !strings.Contains(out, "unknown subcommand") {
		t.Errorf("`x-ui nginx acme-back` → %d %q", code, out)
	}
}
