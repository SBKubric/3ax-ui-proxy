package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
)

// What `x-ui chain …` does on a box, kept here so main.go only parses flags
// and prints (§5.8).

// FetchStatus asks the running hop about itself over its own sub port on the
// loopback, with the hop's own secret. It is the one place the panel-side
// habit of reading the database does not apply: the box's truth lives in the
// running process, not in its config.
func FetchStatus(ctx context.Context, cfg *Config) (*chain.Status, error) {
	if cfg.Bootstrap() {
		return nil, fmt.Errorf("this box has not joined the chain yet — join it at the URL from `x-ui chain join-url`")
	}
	url := cfg.Scheme() + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.SubPort)) + ChainPathPrefix + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.HopSecret)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("`x-ui proxy` does not answer on %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the hop answered %d — is the hopSecret in %s the one it is running with?", resp.StatusCode, cfg.Path())
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return nil, err
	}
	var status chain.Status
	if err := json.Unmarshal(body, &status); err != nil {
		return nil, fmt.Errorf("the hop's status is not a status: %w", err)
	}
	return &status, nil
}

// PrintStatus renders what `x-ui chain status` shows its operator.
func PrintStatus(w io.Writer, status *chain.Status) {
	ports := make([]string, 0, len(status.Relay.Ports))
	for _, port := range status.Relay.Ports {
		ports = append(ports, strconv.Itoa(port))
	}
	fmt.Fprintf(w, "name:      %s (%s)\n", status.Name, status.Role)
	fmt.Fprintf(w, "next hop:  %s:%d (reachable: %t)\n", status.NextHop.Host, status.NextHop.SubPort, status.NextHop.Reachable)
	fmt.Fprintf(w, "revision:  %d%s\n", status.Revision, staleSuffix(status.Stale))
	fmt.Fprintf(w, "relay:     running=%t ports=[%s]\n", status.Relay.Running, strings.Join(ports, " "))
	fmt.Fprintf(w, "last wave: %s\n", formatMilli(status.LastOk))
	if status.ObservedHostMismatch {
		fmt.Fprintln(w, "warning:   the registry's host for this hop differs from this box's domain")
	}
}

func staleSuffix(stale bool) string {
	if stale {
		return " (stale — still relaying)"
	}
	return ""
}

func formatMilli(milli int64) string {
	if milli == 0 {
		return "never"
	}
	return time.UnixMilli(milli).Format(time.RFC3339)
}

// Rejoin points the box at a next hop and joins it there and then, with a
// fresh token from the registry — how a runbook repairs a chain whose inner
// hop died (§4.6, §5.8). The old hop secret is dropped before the attempt:
// after a reissued token it is dead anyway.
func Rejoin(ctx context.Context, cfg *Config, host string, subPort int, scheme, token string) (*JoinResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, fmt.Errorf("chain rejoin: --next-hop is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("chain rejoin: --token is required (reissue it in the panel first)")
	}
	cfg.NextHop.Host = host
	if subPort > 0 {
		cfg.NextHop.SubPort = subPort
	}
	if scheme == "http" || scheme == "https" {
		cfg.NextHop.SubScheme = scheme
	}
	cfg.HopSecret = ""

	state := NewState()
	store := NewDocumentStore(cfg.DocumentPath())
	result, err := Join(ctx, cfg, strings.TrimSpace(token))
	if err != nil {
		// The next hop is worth keeping even when the join failed: the
		// operator fixes the token, not the address, in nearly every case.
		if saveErr := cfg.Save(); saveErr != nil {
			return nil, fmt.Errorf("%w (and the new next hop could not be saved: %v)", err, saveErr)
		}
		return nil, err
	}
	if err := Accept(cfg, state, store, result); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadJoinURL returns the pending join page's URL for `x-ui chain join-url`.
func ReadJoinURL(proxyConfigPath string) (string, error) {
	data, err := os.ReadFile(JoinURLPath(proxyConfigPath))
	if err != nil {
		return "", fmt.Errorf("this box has already joined the chain (or `x-ui proxy` is not running)")
	}
	return strings.TrimSpace(string(data)), nil
}
