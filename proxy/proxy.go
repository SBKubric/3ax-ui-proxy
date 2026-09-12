package proxy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// Run starts the proxy front — the dokodemo-door relay to the real server and,
// when configured, the subscription server that proxies the real panel's /sub
// and /json — then blocks until a termination signal is received.
func Run(cfg *Config) error {
	binPath := xray.GetBinaryPath()
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("xray binary not found at %s (set XUI_BIN_FOLDER): %w", binPath, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Bootstrap mode: no relay manifest yet. Serve the one-time setup page on
	// the subscription port until a manifest is pasted, then carry on as usual
	// in this same process.
	if _, err := os.Stat(cfg.RelayManifestPath); errors.Is(err, fs.ErrNotExist) {
		if err := bootstrap(cfg, sigCh); err != nil {
			return err
		}
		if _, err := os.Stat(cfg.RelayManifestPath); err != nil {
			return nil // interrupted before a manifest arrived
		}
	}

	relay, err := NewRelay(cfg)
	if err != nil {
		return err
	}
	// Backstop: remove the generated relay config on exit. relay.Stop() only
	// deletes it while xray is still running, so if xray dies first it would
	// otherwise linger in the bin folder.
	defer os.Remove(relayConfigPath())

	if err := relay.Start(); err != nil {
		return fmt.Errorf("start relay xray: %w", err)
	}
	logger.Infof("proxy-front: relaying ports %v -> %s via dokodemo-door (L4 passthrough)", relay.Ports(), cfg.UpstreamHost)

	var sub *SubServer
	if cfg.SubEnabled() {
		sub, err = NewSubServer(cfg)
		if err != nil {
			relay.Stop()
			return err
		}
		if err := sub.Start(); err != nil {
			relay.Stop()
			return fmt.Errorf("start sub server: %w", err)
		}
	} else {
		logger.Info("proxy-front: subscription server disabled (no upstreamBase configured); relay only")
	}

	<-sigCh

	logger.Info("proxy-front: shutting down")
	if sub != nil {
		if err := sub.Stop(); err != nil {
			logger.Warning("proxy-front: error stopping sub server:", err)
		}
	}
	if err := relay.Stop(); err != nil {
		logger.Warning("proxy-front: error stopping relay:", err)
	}
	return nil
}

// bootstrap runs the setup page until a manifest is accepted or a signal
// arrives. The setup URL is logged and kept in SetupURLPath(cfg.Path()) for
// the installer and `x-ui proxy-setup-url`; it is removed once spent.
func bootstrap(cfg *Config, sigCh <-chan os.Signal) error {
	setup, err := NewSetupServer(cfg)
	if err != nil {
		return err
	}
	if err := setup.Start(); err != nil {
		return fmt.Errorf("start setup page: %w", err)
	}
	url := setup.URL()
	urlFile := ""
	if cfg.Path() != "" {
		urlFile = SetupURLPath(cfg.Path())
		if err := os.WriteFile(urlFile, []byte(url+"\n"), 0o600); err != nil {
			logger.Warning("proxy-front: cannot record the setup URL:", err)
			urlFile = ""
		}
	}
	logger.Infof("proxy-front: no relay manifest at %s — bootstrap mode. Paste the manifest from the real panel at: %s", cfg.RelayManifestPath, url)
	if urlFile != "" {
		defer os.Remove(urlFile)
	}

	select {
	case <-setup.Accepted():
	case <-sigCh:
		logger.Info("proxy-front: shutting down before any manifest arrived")
	}
	if err := setup.Stop(); err != nil {
		logger.Warning("proxy-front: error stopping setup page:", err)
	}
	return nil
}
