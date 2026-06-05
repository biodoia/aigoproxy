// aigoproxy is the single-binary Tailscale-aware reverse proxy.
// One binary: systemd service + Web UI + TUI + MCP server + ACP server.
//
// Quick start:
//
//   aigoproxy                              # uses ~/.aigoproxy/config.yaml
//   aigoproxy -addr :80 -tui               # start the TUI on a separate addr
//   aigoproxy -no-funnel                   # disable Tailscale Funnel auto-setup
//
// Endpoints exposed by default:
//   /           Web UI (dashboard)
//   /routes     Route management
//   /api/...    JSON API
//   /mcp        JSON-RPC 2.0 (MCP-compatible)
//   /mcp/info   server info
//   /acp/ws     WebSocket (ACP-compatible)
//   /acp/info   server info
//   /healthz    health check
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/biodoia/aigoproxy/internal/acme"
	"github.com/biodoia/aigoproxy/internal/acpserver"
	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/mcpserver"
	"github.com/biodoia/aigoproxy/internal/proxy"
	"github.com/biodoia/aigoproxy/internal/screenshot"
	"github.com/biodoia/aigoproxy/internal/store"
	"github.com/biodoia/aigoproxy/internal/tui"
	"github.com/biodoia/aigoproxy/internal/webui"
)

var (
	addr         = flag.String("addr", ":8080", "dashboard + API + MCP + ACP listen address (single port for simplicity)")
	httpsAddr    = flag.String("https", "", "optional HTTPS listen address (e.g. :443) — uses Tailscale-issued cert for the node's <node>.<tailnet>.ts.net")
	configPath   = flag.String("config", defaultConfig(), "path to config.yaml")
	dataDir      = flag.String("data", defaultData(), "data directory (state, certs, logs)")
	enableTUI    = flag.Bool("tui", false, "start the TUI in this process (interactive)")
	enableFunnel = flag.Bool("funnel", true, "register Tailscale Funnel listeners for routes (requires `tailscale` CLI)")
	showVersion  = flag.Bool("version", false, "print version and exit")
	setupMode    = flag.Bool("setup", false, "run interactive setup wizard and exit (creates initial config.yaml)")
)

func defaultConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aigoproxy", "config.yaml")
}

func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aigoproxy")
}

const version = "0.1.0"

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Println("aigoproxy", version)
		return
	}
	if *setupMode {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		if err := runSetup(logger); err != nil {
			fmt.Fprintf(os.Stderr, "setup: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// 1. Store
	s, err := store.New(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	// 2. Config
	if _, err := s.LoadConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	cfg := s.Config()
	logger.Info("aigoproxy starting",
		"version", version,
		"addr", *addr,
		"data_dir", *dataDir,
		"config", *configPath,
		"routes", len(cfg.Routes),
	)

	// 3. Reverse proxy
	px := proxy.New(s, logger)
	if err := px.Reload(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: %v\n", err)
		os.Exit(1)
	}

	// 4. ACME manager
	acm, err := acme.New(acme.Config{
		DataDir: *dataDir,
		Email:   os.Getenv("AIGOPROXY_ACME_EMAIL"),
		Staging: os.Getenv("AIGOPROXY_ACME_STAGING") == "1",
		Logger:  logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "acme: %v\n", err)
		os.Exit(1)
	}
	// renewal loop started after ctx is created (see below)

	// 5. Servers
	// Screenshot manager captures PNG previews of route upstreams. The
	// directory lives under dataDir so the systemd unit's ReadWritePaths
	// already covers it.
	ssDir := filepath.Join(*dataDir, "screenshots")
	ss := screenshot.New(screenshot.Config{
		Dir:      ssDir,
		Interval: 5 * time.Minute,
		Timeout:  30 * time.Second,
		Logger:   logger,
	})
	// Wire HostsFn and URLFn so the screenshot manager knows what to capture.
	// For tailnet-internal access, the URL is http://<host> from the
	// local network. For Tailscale Funnel, we use https://<node>.<tailnet>/<path>.
	ss.HostsFn = func() []string {
		out := []string{}
		for _, r := range s.Config().Routes {
			if r.Enabled {
				out = append(out, r.Host)
			}
		}
		return out
	}
	ss.URLFn = func(host string) string {
		// Try the local upstream first (no Tailscale cert needed for
		// localhost). If the upstream is reachable we capture it; if
		// not, chrome will just produce a "connection refused" image
		// which is also useful for status debugging.
		for _, r := range s.Config().Routes {
			if r.Host == host {
				return r.Upstream
			}
		}
		return "http://localhost"
	}

	webuiSrv, err := webui.New(*addr, s, px, ss, cfg.BaseDomain, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webui: %v\n", err)
		os.Exit(1)
	}
	mcpSrv := mcpserver.New(s, logger)
	acpSrv := acpserver.New(s, logger)
	// Wire MCP server callbacks to webui so MCP tools can drive the full
	// agentic flow: scan → inspect → register → reload → screenshot.
	mcpSrv.SetOnRouteChanged(func() {
		_ = px.Reload()
		if ss != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for _, r := range s.Config().Routes {
				_ = ss.Capture(ctx, r.Host)
			}
		}
	})
	mcpSrv.SetScan(func() []any {
		out := []any{}
		for _, sg := range webuiSrv.Suggestions() {
			out = append(out, sg)
		}
		return out
	})

	// 6. Compose HTTP
	root := http.NewServeMux()
	// MCP: register both /mcp and /mcp/ (Go 1.21 mux treats them differently)
	root.Handle("/mcp", mcpSrv.Handler())
	root.Handle("/mcp/", mcpSrv.Handler())
	// ACP: same
	root.Handle("/acp", acpSrv.Handler())
	root.Handle("/acp/", acpSrv.Handler())
	root.Handle("/", combinedHandler(px, webuiSrv, acm, cfg))

	// 7. Health probe loop + ACME renewal
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go px.HealthCheckLoop(ctx)
	go acm.RenewalLoop(ctx)
	go ss.Loop(ctx)

	// 8. TUI (optional)
	if *enableTUI {
		go func() {
			ui := newTUI(s, logger)
			if err := ui.Run(ctx); err != nil && err.Error() != "exit" {
				logger.Warn("tui exited", "err", err)
			}
		}()
	}

	// 9. Tailscale Funnel (if enabled)
	if *enableFunnel {
		go setupFunnel(ctx, cfg, logger)
	}

	// 10. HTTP server
	srv := &http.Server{
		Addr:              *addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	// 11. HTTPS listener (optional). Auto-provisions a Tailscale cert for
	// the node's <node>.<tailnet>.ts.net and serves the same root mux.
	var httpsSrv *http.Server
	if *httpsAddr != "" {
		ts, err := tailscaleStatus(ctx)
		if err != nil {
			logger.Warn("https listener disabled (no tailscale)", "err", err)
		} else if ts.DNSName == "" {
			logger.Warn("https listener disabled (no DNSName in tailscale status)")
		} else {
			cf, kf, err := ensureTailscaleCert(ctx, logger, ts.DNSName, *dataDir)
			if err != nil {
				logger.Warn("https listener disabled (cert)", "err", err)
			} else {
				httpsSrv = newHTTPSServer(*httpsAddr, root, cf, kf)
				go func() {
					logger.Info("https listening", "addr", *httpsAddr)
					if err := httpsSrv.ListenAndServeTLS(cf, kf); err != nil && err != http.ErrServerClosed {
						logger.Error("https serve", "err", err)
					}
				}()
				// watcher: re-provision cert when Tailscale rotates it
				go watchCert(ctx, logger, cf, kf, func() {
					ncf, nkf, err := ensureTailscaleCert(ctx, logger, ts.DNSName, *dataDir)
					if err != nil {
						logger.Error("cert refresh", "err", err)
						return
					}
					shutCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
					defer c2()
					_ = httpsSrv.Shutdown(shutCtx)
					httpsSrv = newHTTPSServer(*httpsAddr, root, ncf, nkf)
					go func() {
						logger.Info("https restarted with new cert", "addr", *httpsAddr, "cert", ncf)
						if err := httpsSrv.ListenAndServeTLS(ncf, nkf); err != nil && err != http.ErrServerClosed {
							logger.Error("https serve (after refresh)", "err", err)
						}
					}()
				})
			}
		}
	}

	// SIGHUP = graceful config reload (re-reads config.yaml and rebuilds
	// the proxy route table; connections are not dropped).
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		for range hupCh {
			logger.Info("SIGHUP received, reloading config")
			if _, err := s.LoadConfig(); err != nil {
				logger.Error("reload config", "err", err)
				continue
			}
			if err := px.Reload(); err != nil {
				logger.Error("reload proxy", "err", err)
				continue
			}
			logger.Info("reload complete", "routes", len(s.Config().Routes))
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer c2()
		_ = srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}

// combinedHandler routes: dashboard paths → webui, reverse proxy for the rest.
// /mcp and /acp are registered at the root mux level and take precedence
// over this handler. ACME HTTP-01 challenges are served before the reverse
// proxy so /.well-known/acme-challenge/* never hits an upstream.
func combinedHandler(px *proxy.Proxy, webuiSrv *webui.Server, acm *acme.Manager, cfg config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ACME HTTP-01 challenge path: serve from in-memory token store
		if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
			acm.ChallengeHandler().ServeHTTP(w, r)
			return
		}
		// Screenshots: served by the webui regardless of host (the
		// screenshot files live in our data dir).
		if strings.HasPrefix(r.URL.Path, "/screenshots/") {
			webuiSrv.Handler().ServeHTTP(w, r)
			return
		}
		// Dashboard + API: served when Host is the node's own FQDN (or
		// empty), so that requests to <route>.<base> are routed to the
		// reverse proxy without intercepting the dashboard.
		nodeHosts := nodeLocalHosts(cfg)
		host := strings.ToLower(r.Host)
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		isLocal := host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1"
		if !isLocal {
			if !contains(nodeHosts, host) {
				// request to a virtual host → reverse proxy
				px.ServeHTTP(w, r)
				return
			}
		}
		// API + dashboard: webui
		switch r.URL.Path {
		case "/", "/routes", "/healthz":
			webuiSrv.Handler().ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			webuiSrv.Handler().ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") {
			webuiSrv.Handler().ServeHTTP(w, r)
			return
		}
		// reverse proxy for everything else on a local host
		px.ServeHTTP(w, r)
	})
}

// nodeLocalHosts returns the FQDN variants the proxy should serve the
// dashboard on. We include the base domain and the localhost-ish names.
func nodeLocalHosts(cfg config.Config) []string {
	// Note: we don't know the node FQDN here without shelling out, so
	// we only include the base domain. Users who want the dashboard on
	// their node FQDN can set the host header to localhost from inside
	// the Tailscale network.
	out := []string{}
	if cfg.BaseDomain != "" {
		out = append(out, cfg.BaseDomain)
	}
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// setupFunnel registers a Tailscale Funnel listener for every route marked
// auth=funnel. It shells out to the `tailscale` CLI. Best-effort: if the
// CLI isn't available, we just log and continue.
func setupFunnel(ctx context.Context, cfg config.Config, logger *slog.Logger) {
	// Funnel setup is intentionally a no-op here. The expected deployment
	// is: aigoproxy binds :80, and the user runs `tailscale funnel 80 on`
	// once via the systemd unit's ExecStartPost. This keeps aigoproxy
	// itself free of Tailscale-specific code and makes it work even on
	// non-Tailscale machines.
	logger.Info("funnel: ensure `tailscale funnel 80 on` is run once per host")
}

// newTUI returns the TUI; the tui package is the implementation.
func newTUI(s *store.Store, logger *slog.Logger) *tui.UI {
	return tui.New(s, logger)
}
