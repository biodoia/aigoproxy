// Package proxy implements the HTTP/HTTPS reverse proxy core of aigoproxy.
//
// The proxy dispatches incoming requests to upstreams based on the Host
// header. It is transport-agnostic: it works equally well on plain HTTP
// (mode A: Tailscale Funnel) and on HTTPS (mode B: aigoproxy terminates
// TLS via ACME-issued certs).
//
// Concurrency: Proxy holds a read-only snapshot of routes at Serve time
// via Store. Re-issuing routes from the API requires calling Reload.
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

// Proxy is the reverse proxy.
type Proxy struct {
	store  *store.Store
	logger *slog.Logger
	mu     sync.RWMutex
	// routes is a cached map host → *httputil.ReverseProxy
	routes map[string]*routeEntry
	// healthStatus is a per-route atomic health flag (0 = down, 1 = up).
	healthStatus sync.Map
	// httpClient is shared for upstream requests (and health checks)
	httpClient *http.Client
	// routeStats is host → per-route runtime counters (active conns, total
	// requests, total bytes, last status, last request time).
	routeStats sync.Map
}

// RouteStats is the runtime stats surface for a single route.
type RouteStats struct {
	ActiveConns  int64     `json:"active_conns"`
	TotalReqs    int64     `json:"total_requests"`
	TotalBytes   int64     `json:"total_bytes"`
	LastStatus   int       `json:"last_status"`
	LastRequest  time.Time `json:"last_request"`
	LastLatency  int64     `json:"last_latency_ms"`
	FirstRequest time.Time `json:"first_request,omitempty"`
}

// GetStats returns a snapshot of stats for host.
func (p *Proxy) GetStats(host string) RouteStats {
	v, ok := p.routeStats.Load(host)
	if !ok {
		return RouteStats{}
	}
	s := v.(*atomicRouteStats)
	return s.snapshot()
}

// AllStats returns stats for every route currently registered.
func (p *Proxy) AllStats() map[string]RouteStats {
	out := map[string]RouteStats{}
	p.routeStats.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomicRouteStats).snapshot()
		return true
	})
	return out
}

// atomicRouteStats is a small struct of atomics for hot-path counters.
type atomicRouteStats struct {
	activeConns atomic.Int64
	totalReqs   atomic.Int64
	totalBytes  atomic.Int64
	lastStatus  atomic.Int64 // int for CAS-free store
	lastReqUnix atomic.Int64
	lastLatency atomic.Int64
	firstReq    atomic.Int64
}

func (s *atomicRouteStats) snapshot() RouteStats {
	lr := s.lastReqUnix.Load()
	fr := s.firstReq.Load()
	r := RouteStats{
		ActiveConns: s.activeConns.Load(),
		TotalReqs:   s.totalReqs.Load(),
		TotalBytes:  s.totalBytes.Load(),
		LastStatus:  int(s.lastStatus.Load()),
		LastLatency: s.lastLatency.Load(),
	}
	if lr > 0 {
		r.LastRequest = time.Unix(0, lr)
	}
	if fr > 0 {
		r.FirstRequest = time.Unix(0, fr)
	}
	return r
}

func (p *Proxy) statsFor(host string) *atomicRouteStats {
	if v, ok := p.routeStats.Load(host); ok {
		return v.(*atomicRouteStats)
	}
	v, _ := p.routeStats.LoadOrStore(host, &atomicRouteStats{})
	return v.(*atomicRouteStats)
}

type routeEntry struct {
	cfg   config.Route
	proxy *httputil.ReverseProxy
}

// New returns a new Proxy.
func New(s *store.Store, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		store:  s,
		logger: logger,
		routes: make(map[string]*routeEntry),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	return p
}

// Reload rebuilds the internal route table from the store.
func (p *Proxy) Reload() error {
	cfg := p.store.Config()
	entries := make(map[string]*routeEntry, len(cfg.Routes))
	for _, r := range cfg.Routes {
		if !r.Enabled {
			continue
		}
		upstream, err := url.Parse(r.Upstream)
		if err != nil {
			p.logger.Error("skip route: bad upstream", "host", r.Host, "upstream", r.Upstream, "err", err)
			continue
		}
		entry := &routeEntry{cfg: r}
		// FlushInterval: -1 disables response buffering, which is
		// required for streaming protocols (WebSocket, SSE, large
		// downloads, etc.) so bytes flow through to the client as
		// soon as the upstream writes them.
		//
		// The default Transport (http.DefaultTransport) already does
		// hop-by-hop header stripping (Connection, Upgrade, etc.)
		// and protocol-bridging for WebSocket — Go's stdlib
		// ReverseProxy supports WS out of the box, as long as
		// FlushInterval is set to a negative value.
		entry.proxy = &httputil.ReverseProxy{
			Director:       p.makeDirector(r, upstream),
			Transport:      p.httpClient.Transport,
			ErrorHandler:   p.errorHandler(r),
			FlushInterval:  -1,
		}
		entries[r.Host] = entry
	}
	p.mu.Lock()
	p.routes = entries
	p.mu.Unlock()
	p.logger.Info("proxy: routes reloaded", "count", len(entries))
	return nil
}

// makeDirector returns the director function for a route, configuring
// scheme, host, and path rewriting.
func (p *Proxy) makeDirector(r config.Route, upstream *url.URL) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = upstream.Scheme
		req.URL.Host = upstream.Host
		req.Host = upstream.Host // Important: don't leak the original Host header
		// path prefix stripping
		if r.StripPrefix != "" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, r.StripPrefix)
			if !strings.HasPrefix(req.URL.Path, "/") {
				req.URL.Path = "/" + req.URL.Path
			}
		}
		// mark request as proxied
		req.Header.Set("X-Forwarded-Host", r.Host)
		req.Header.Set("X-Forwarded-Proto", "https")
		if existing := req.Header.Get("X-Forwarded-For"); existing != "" {
			req.Header.Set("X-Forwarded-For", existing+", "+req.RemoteAddr)
		} else {
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		}
	}
}

// errorHandler logs upstream failures and writes a 502 to the client.
func (p *Proxy) errorHandler(r config.Route) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, req *http.Request, err error) {
		p.logger.Warn("upstream error", "host", r.Host, "path", req.URL.Path, "err", err)
		// mark route unhealthy
		p.healthStatus.Store(r.Host, atomic.Bool{})
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, fmt.Sprintf("upstream %s unreachable: %v\n", r.Upstream, err))
	}
}

// ServeHTTP routes the request based on Host header.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	start := time.Now()

	// Check health-status for known routes
	entry := p.lookup(host, r.URL.Path)
	if entry == nil {
		// Unknown host — serve the dashboard
		p.serveDashboard(w, r)
		p.logAccess(host, r, http.StatusNotFound, 0, time.Since(start), "")
		return
	}
	// Track this connection
	stats := p.statsFor(entry.cfg.Host)
	stats.activeConns.Add(1)
	stats.totalReqs.Add(1)
	if stats.firstReq.Load() == 0 {
		stats.firstReq.Store(time.Now().UnixNano())
	}
	defer stats.activeConns.Add(-1)

	// Auth check
	if !p.checkAuth(entry.cfg, r) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "forbidden\n")
		p.logAccess(host, r, http.StatusForbidden, 0, time.Since(start), "")
		stats.lastStatus.Store(int64(http.StatusForbidden))
		stats.lastReqUnix.Store(time.Now().UnixNano())
		stats.lastLatency.Store(time.Since(start).Milliseconds())
		return
	}

	// Health probe
	if entry.cfg.Health != "" && r.URL.Path == entry.cfg.Health {
		w.Header().Set("Content-Type", "application/json")
		if p.isHealthy(host) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"unhealthy"}`)
		}
		return
	}

	// Capture status code for logging
	rw := &statusRecorder{ResponseWriter: w, status: 200}
	entry.proxy.ServeHTTP(rw, r)

	dur := time.Since(start)
	p.logAccess(host, r, rw.status, rw.bytes, dur, r.Header.Get("Tailscale-User"))
	// Update per-route stats
	stats.lastStatus.Store(int64(rw.status))
	stats.totalBytes.Add(rw.bytes)
	stats.lastReqUnix.Store(time.Now().UnixNano())
	stats.lastLatency.Store(dur.Milliseconds())
}

// lookup finds a route for the given host. If a path prefix is given
// (for routes registered behind a Tailscale Funnel set-path), it tries
// the longest matching path first. Falls back to exact host match.
func (p *Proxy) lookup(host, path string) *routeEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// 1. exact host match (works for tailnet-internal access)
	if e, ok := p.routes[host]; ok {
		return e
	}
	// 2. path-based routing (for Tailscale Funnel: all services behind
	//    a single :443, distinguished by URL path prefix)
	if path != "" {
		var bestMatch string
		var bestEntry *routeEntry
		for _, e := range p.routes {
			if e.cfg.PathPrefix == "" {
				continue
			}
			if strings.HasPrefix(path, e.cfg.PathPrefix) {
				if len(e.cfg.PathPrefix) > len(bestMatch) {
					bestMatch = e.cfg.PathPrefix
					bestEntry = e
				}
			}
		}
		if bestEntry != nil {
			return bestEntry
		}
	}
	return nil
}

// isHealthy returns the cached health status (defaults to true if no probe yet).
func (p *Proxy) isHealthy(host string) bool {
	v, ok := p.healthStatus.Load(host)
	if !ok {
		return true // no data yet
	}
	b := v.(*atomic.Bool)
	return b.Load()
}

// checkAuth returns true if the request is allowed by the route's auth setting.
func (p *Proxy) checkAuth(r config.Route, req *http.Request) bool {
	switch r.Auth {
	case "none", "":
		return true
	case "tailscale":
		// Tailscale injects these headers when the request is from a tailnet device
		return req.Header.Get("Tailscale-User") != "" || req.Header.Get("X-Forwarded-For-Tailscale") != ""
	case "funnel":
		// Funnel is allowed; Tailscale Funnel proxies to localhost and may not
		// set Tailscale-User. We trust the network path here.
		return true
	}
	return false
}

// serveDashboard shows a minimal HTML page listing the routes, useful
// for the "I navigated to the wrong host" case.
func (p *Proxy) serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, "aigoproxy\nno route for host %q\n\nregistered hosts:\n", r.Host)
	for _, route := range p.store.Config().Routes {
		if route.Enabled {
			_, _ = fmt.Fprintf(w, "  %s → %s\n", route.Host, route.Upstream)
		}
	}
}

// logAccess is a small helper that wraps store.LogAccess.
func (p *Proxy) logAccess(host string, r *http.Request, status int, bytes int64, dur time.Duration, user string) {
	p.store.LogAccess(store.AccessLogEntry{
		Time:      time.Now(),
		Host:      host,
		Method:    r.Method,
		Path:      r.URL.Path,
		Status:    status,
		Bytes:     bytes,
		LatencyMs: dur.Milliseconds(),
		Remote:    clientIP(r),
		User:      user,
	})
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		// first hop
		if i := strings.Index(h, ","); i > 0 {
			return strings.TrimSpace(h[:i])
		}
		return strings.TrimSpace(h)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func stripPort(h string) string {
	if i := strings.IndexByte(h, ':'); i > 0 {
		return h[:i]
	}
	return h
}

// statusRecorder wraps ResponseWriter to capture status + bytes.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(c int) {
	if !s.wroteHeader {
		s.status = c
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush forwards to underlying writer for streaming responses.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so the underlying ReverseProxy can
// upgrade the connection to a different protocol (most importantly
// WebSocket). Without this, ws:// requests fail with
// "can't switch protocols using non-Hijacker ResponseWriter type".
//
// We capture the hijacked status as "switching protocols" (101) so
// the access log records the upgrade attempt.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if !s.wroteHeader {
		s.status = http.StatusSwitchingProtocols
		s.wroteHeader = true
	}
	return hj.Hijack()
}

// HealthCheckLoop runs periodic health probes for all routes in the background.
func (p *Proxy) HealthCheckLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.probeAll()
		}
	}
}

func (p *Proxy) probeAll() {
	cfg := p.store.Config()
	for _, r := range cfg.Routes {
		if !r.Enabled || r.Health == "" {
			continue
		}
		go p.probe(r)
	}
}

func (p *Proxy) probe(r config.Route) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := strings.TrimRight(r.Upstream, "/") + r.Health
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := p.httpClient.Do(req)
	healthy := err == nil && resp != nil && resp.StatusCode < 500
	if resp != nil {
		resp.Body.Close()
	}
	v, _ := p.healthStatus.LoadOrStore(r.Host, &atomic.Bool{})
	v.(*atomic.Bool).Store(healthy)
}
