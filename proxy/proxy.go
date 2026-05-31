package proxy

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// Run starts the proxy front — the dokodemo-door relay to the real server — and
// blocks until a termination signal is received. The subscription server (live
// fetch from the real panel + custom page) is added on top of this in a later
// step.
func Run(cfg *Config) error {
	binPath := xray.GetBinaryPath()
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("xray binary not found at %s (set XUI_BIN_FOLDER): %w", binPath, err)
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

	// TODO: start the subscription server here — live-fetch /sub/:id from
	// cfg.UpstreamSubURL and render the custom proxy page (apps + copy-JSON).

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("proxy-front: shutting down")
	if err := relay.Stop(); err != nil {
		logger.Warning("proxy-front: error stopping relay:", err)
	}
	return nil
}
