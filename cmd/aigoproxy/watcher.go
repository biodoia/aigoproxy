// watcher.go — poll-based file watcher for the Tailscale cert file.
// We use polling instead of fsnotify to keep deps minimal: tailscale
// certs are renewed at most every 60 days, so a 1-minute poll is
// plenty.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// watchCert polls the cert file every minute and calls onChange whenever
// the file mtime updates. Stops when ctx is cancelled.
func watchCert(ctx context.Context, logger *slog.Logger, certPath, keyPath string, onChange func()) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	var lastMod time.Time
	if info, err := os.Stat(certPath); err == nil {
		lastMod = info.ModTime()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			info, err := os.Stat(certPath)
			if err != nil {
				continue // cert missing — will retry
			}
			if !info.ModTime().Equal(lastMod) {
				logger.Info("cert file changed, reloading",
					"cert", certPath, "key", keyPath,
					"old_mtime", lastMod.Format(time.RFC3339),
					"new_mtime", info.ModTime().Format(time.RFC3339),
				)
				lastMod = info.ModTime()
				onChange()
			}
		}
	}
}
