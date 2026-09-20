package proxy

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// Run starts this hop of the proxy chain: the sub port (subscriptions, the
// wave endpoints and — until the box has joined — the join page), the
// dokodemo-door relay to the next hop, and the wave client that keeps both in
// step with the chain document. It blocks until a termination signal.
//
// There is no mode switch after startup: the sub port comes up once and stays
// up, and joining only fills in what the hop did not know yet.
func Run(cfg *Config) error {
	for _, warning := range cfg.LegacyWarnings() {
		logger.Warning(warning)
	}

	binPath := xray.GetBinaryPath()
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("xray binary not found at %s (set XUI_BIN_FOLDER): %w", binPath, err)
	}

	state := NewState()
	store := NewDocumentStore(cfg.DocumentPath())
	// The cached document is what lets a rebooted box relay before the first
	// poll comes back (§3.6).
	if doc, err := store.Load(); err != nil {
		logger.Warning("proxy-front:", err)
	} else if doc != nil {
		state.SetDocument(doc)
		logger.Infof("proxy-front: resuming from the cached chain document, revision %d", doc.Revision)
	}

	relay := NewRelay(cfg.RelayListen)
	defer relay.Stop()
	poller := NewPoller(cfg, state, store, relay)

	var join *JoinPage
	if cfg.Bootstrap() {
		var err error
		if join, err = NewJoinPage(cfg, state, store); err != nil {
			return err
		}
	}

	sub, err := NewSubServer(cfg, state, NewChainHandler(cfg, state, relay), join)
	if err != nil {
		return err
	}
	if err := sub.Start(); err != nil {
		return fmt.Errorf("start sub server: %w", err)
	}
	defer sub.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if join != nil {
		if !awaitJoin(cfg, join, sigCh) {
			logger.Info("proxy-front: shutting down before this box joined the chain")
			return nil
		}
		poller.SetPollSeconds(join.Result().PollSeconds)
		poller.MarkJoined()
	}

	if doc := state.Document(); doc != nil {
		if err := relay.Apply(doc.Ports, doc.NextHop.Host); err != nil {
			// A relay that cannot start is not a reason to stop the wave:
			// the next revision may be the one that fixes it.
			logger.Error("proxy-front: starting the relay failed:", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)

	<-sigCh
	logger.Info("proxy-front: shutting down")
	cancel()
	if err := sub.Stop(); err != nil {
		logger.Warning("proxy-front: error stopping sub server:", err)
	}
	if err := relay.Stop(); err != nil {
		logger.Warning("proxy-front: error stopping relay:", err)
	}
	return nil
}

// awaitJoin serves the join page until this box joins or a signal arrives. The
// URL is logged and kept in JoinURLPath(cfg.Path()) for the installer and
// `x-ui chain join-url`; it is removed once the page is spent.
func awaitJoin(cfg *Config, join *JoinPage, sigCh <-chan os.Signal) bool {
	url := join.URL()
	urlFile := ""
	if cfg.Path() != "" {
		urlFile = JoinURLPath(cfg.Path())
		if err := os.WriteFile(urlFile, []byte(url+"\n"), 0o600); err != nil {
			logger.Warning("proxy-front: cannot record the join URL:", err)
			urlFile = ""
		}
	}
	logger.Infof("proxy-front: no hopSecret in %s — bootstrap mode. Join this box at: %s", cfg.Path(), url)
	if !cfg.TLS() {
		logger.Warning("proxy-front: " + insecureJoinWarning)
	}

	select {
	case <-join.Accepted():
		if urlFile != "" {
			os.Remove(urlFile)
		}
		return true
	case <-sigCh:
		return false
	}
}
