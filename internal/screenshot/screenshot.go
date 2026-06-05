// Package screenshot captures PNG previews of route upstreams using
// headless Chrome. The standard pattern for "is this site up?" dashboards.
//
// We use the locally-installed Google Chrome (/opt/google/chrome/chrome) at
// the system level. No new dependency.
//
// The capture is async: a goroutine refreshes one screenshot at a time
// every ScreenshotInterval. Clients fetch the latest image via
// /screenshots/<host>.png; a 404 means "not yet captured" or "capture
// failed".
package screenshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Manager owns the screenshot directory and the refresh loop.
type Manager struct {
	dir      string
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger
	chrome   string

	mu     sync.Mutex
	latest map[string]time.Time // host → last successful capture

	// hooks: set by main
	HostsFn func() []string // returns the list of hostnames to capture
	URLFn   func(host string) string // returns the URL to visit (depends on host/path routing)
}

// Config configures the Manager.
type Config struct {
	Dir      string        // directory to store PNGs
	Interval time.Duration // how often to refresh (default 5m)
	Timeout  time.Duration // per-capture timeout (default 30s)
	Logger   *slog.Logger
	Chrome   string // path to chrome (default /opt/google/chrome/chrome)
}

// New creates a Manager.
func New(cfg Config) *Manager {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Chrome == "" {
		cfg.Chrome = "/opt/google/chrome/chrome"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{
		dir:      cfg.Dir,
		interval: cfg.Interval,
		timeout:  cfg.Timeout,
		logger:   cfg.Logger,
		chrome:   cfg.Chrome,
		latest:   map[string]time.Time{},
	}
}

// PathFor returns the absolute path of the screenshot for host, or "" if
// not yet captured.
func (m *Manager) PathFor(host string) string {
	p := filepath.Join(m.dir, host+".png")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// LastCapture returns when host was last successfully captured.
func (m *Manager) LastCapture(host string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest[host]
}

// Loop runs the refresh loop. Cancelled by ctx.
func (m *Manager) Loop(ctx context.Context) {
	// initial pass
	m.refreshAll(ctx)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.refreshAll(ctx)
		}
	}
}

func (m *Manager) refreshAll(ctx context.Context) {
	if m.HostsFn == nil {
		return
	}
	for _, h := range m.HostsFn() {
		if err := m.Capture(ctx, h); err != nil {
			m.logger.Warn("screenshot capture", "host", h, "err", err)
		}
	}
}

// Capture forces a fresh capture of host, saves it, and updates the
// timestamp.
func (m *Manager) Capture(ctx context.Context, host string) error {
	if m.URLFn == nil {
		return errors.New("screenshot: URLFn not set")
	}
	url := m.URLFn(host)
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(m.dir, host+".png")
	c, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--hide-scrollbars",
		"--disable-dev-shm-usage",
		"--window-size=1280,800",
		"--virtual-time-budget=10000", // cap page load at 10s
		"--screenshot=" + out,
		url,
	}
	cmd := exec.CommandContext(c, m.chrome, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chrome: %w (stderr: %s)", err, stderr.String())
	}
	info, err := os.Stat(out)
	if err != nil {
		return err
	}
	if info.Size() < 100 {
		return fmt.Errorf("screenshot too small (%d bytes) — page probably failed to load", info.Size())
	}
	m.mu.Lock()
	m.latest[host] = time.Now()
	m.mu.Unlock()
	m.logger.Info("screenshot captured", "host", host, "size", info.Size())
	return nil
}
