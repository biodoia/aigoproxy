// Package webui serves the aigoproxy dashboard. Stdlib-only, anti-slop
// dark theme, 2026 card-based design.
//
// Endpoints (all relative to aigoproxy root):
//   /                       — dashboard with route cards + screenshots
//   /routes                 — add/edit route form
//   /api/routes             — REST CRUD on routes
//   /api/log                — recent access log (JSON)
//   /api/stats              — runtime stats
//   /api/active-conns       — current open connections
//   /api/recapture          — force a fresh screenshot for a host (POST {host})
//   /api/recapture-all      — refresh all screenshots (POST)
//   /api/rescan             — scan local ports for new services (POST)
//   /api/enable-funnel      — set Tailscale Funnel for a path (POST {host,path})
//   /api/suggestions        — last port scan results
//   /screenshots/<h>.png     — cached screenshot
//   /healthz                — health probe
//   /mcp, /acp/*            — JSON-RPC endpoints
//   /static/*               — css/js
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/detector"
	"github.com/biodoia/aigoproxy/internal/ports"
	"github.com/biodoia/aigoproxy/internal/proxy"
	"github.com/biodoia/aigoproxy/internal/screenshot"
	"github.com/biodoia/aigoproxy/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// urlPort extracts the port from an upstream URL. Returns -1 on error.
func urlPort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return -1, err
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		// Hostname without port; treat as 80
		if u.Scheme == "https" {
			return 443, nil
		}
		return 80, nil
	}
	return strconvAtoi(portStr)
}

// netDialerType is the underlying dialer interface used in rescan.
type netDialerType struct {
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// netDialer is the package-level dialer used for rescan probes. It
// defaults to net.Dialer but can be overridden in tests.
var netDialer = netDialerType{
	DialContext: (&net.Dialer{}).DialContext,
}

// strconvAtoi is a tiny indirection to keep imports clean.
func strconvAtoi(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// Server is the dashboard HTTP server.
type Server struct {
	addr   string
	store  *store.Store
	px     *proxy.Proxy
	ss     *screenshot.Manager
	ports  *ports.Allocator
	logger *slog.Logger
	tmpl   *template.Template

	// lastRescan is the result of the most recent /api/rescan call.
	lastRescan []Suggestion
	// baseDomain is used to derive suggested hostnames.
	baseDomain string
}

// Suggestion is a port-scan finding the user can register.
type Suggestion struct {
	Port           int    `json:"port"`
	TitleGuess     string `json:"title_guess"`
	FirstByte      string `json:"first_byte"`
	HasAuth        bool   `json:"has_auth"`
	AuthReason     string `json:"auth_reason,omitempty"`
	RecommendedAuth string `json:"recommended_auth"`
}

// New returns a new Server.
func New(addr string, s *store.Store, px *proxy.Proxy, ss *screenshot.Manager, portAlloc *ports.Allocator, baseDomain string, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	t, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		addr:       addr,
		store:      s,
		px:         px,
		ss:         ss,
		ports:      portAlloc,
		logger:     logger,
		tmpl:       t,
		baseDomain: baseDomain,
	}, nil
}

// Handler returns the http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/routes", s.handleRoutes)
	mux.HandleFunc("/services", s.handleServices)
	mux.HandleFunc("/ports", s.handlePorts)
	mux.HandleFunc("/api/routes", s.handleAPIRoutes)
	mux.HandleFunc("/api/log", s.handleAPILog)
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/api/active-conns", s.handleAPIActiveConns)
	mux.HandleFunc("/api/recapture", s.handleAPIRecapture)
	mux.HandleFunc("/api/recapture-all", s.handleAPIRecaptureAll)
	mux.HandleFunc("/api/rescan", s.handleAPIRescan)
	mux.HandleFunc("/api/enable-funnel", s.handleAPIEnableFunnel)
	mux.HandleFunc("/api/suggestions", s.handleAPISuggestions)
	mux.HandleFunc("/api/ports/list", s.handleAPIPortsList)
	mux.HandleFunc("/api/ports/claim", s.handleAPIPortsClaim)
	mux.HandleFunc("/api/ports/release", s.handleAPIPortsRelease)
	mux.HandleFunc("/screenshots/", s.handleScreenshot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/static/", s.handleStatic)
	return s.recoverMW(s.logMW(mux))
}

// routeView is the per-route data passed to the dashboard template.
type routeView struct {
	Host           string
	Upstream       string
	PathPrefix     string
	Auth           string
	Health         string
	Enabled        bool
	Healthy        bool
	ScreenshotReady bool
	LastStatus     int
	LastStatusClass string
	TotalReqs      int64
	TotalBytesHuman string
	ActiveConns    int64
	LastLatency    int64
	LastRequestUnix string
	LastSeen       string
}

func (s *Server) routesView() []routeView {
	cfg := s.store.Config()
	allStats := s.px.AllStats()
	out := make([]routeView, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		st := allStats[r.Host]
		v := routeView{
			Host:            r.Host,
			Upstream:        r.Upstream,
			PathPrefix:      r.PathPrefix,
			Auth:            r.Auth,
			Health:          r.Health,
			Enabled:         r.Enabled,
			Healthy:         st.LastStatus >= 200 && st.LastStatus < 400 && st.TotalReqs > 0,
			ScreenshotReady:  s.ss != nil && s.ss.PathFor(r.Host) != "",
			LastStatus:      int(st.LastStatus),
			LastStatusClass: statusClass(int(st.LastStatus)),
			TotalReqs:       st.TotalReqs,
			TotalBytesHuman: humanBytes(st.TotalBytes),
			ActiveConns:     st.ActiveConns,
			LastLatency:     st.LastLatency,
			LastRequestUnix: unixNanoString(st.LastRequest),
			LastSeen:       relativeTime(st.LastRequest),
		}
		out = append(out, v)
	}
	return out
}

func statusClass(s int) string {
	switch {
	case s == 0:
		return "0"
	case s < 200:
		return "0"
	case s < 300:
		return "2xx"
	case s < 400:
		return "3xx"
	case s < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(n)/1024/1024)
	default:
		return fmt.Sprintf("%.1fG", float64(n)/1024/1024/1024)
	}
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// unixNanoString renders t.UnixNano() as a string, or "0" for the zero
// value (used by the dashboard's data-lastreq attribute).
func unixNanoString(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d", t.UnixNano())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := struct {
		Title       string
		Stats       store.Stats
		Routes      []routeView
		Suggestions []Suggestion
		Now         time.Time
	}{
		Title:       "aigoproxy",
		Stats:       s.store.Stats(),
		Routes:      s.routesView(),
		Suggestions: s.lastRescan,
		Now:         time.Now(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.logger.Error("template", "err", err)
	}
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/routes" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "routes.html", struct {
		Title  string
		Routes []routeView
	}{Title: "aigoproxy — routes", Routes: s.routesView()}); err != nil {
		s.logger.Error("template", "err", err)
	}
}

// handleServices serves the static catalog of FGT services at /services.
// The page is a pre-rendered HTML file embedded from the static/ dir.
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/services.html")
	if err != nil {
		http.Error(w, "services page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handlePorts shows the Memogo-backed port-allocator state at /ports.
// Lists every reservation (port, owner, allocated_at, note) and the
// "interesting" free ports — useful when an agent is about to start
// a new service and needs to know what's available without scanning.
func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	res, err := s.ports.List(r.Context())
	if err != nil {
		s.logger.Error("ports: list", "err", err)
		http.Error(w, "port-allocator unreachable", http.StatusBadGateway)
		return
	}
	data := struct {
		Reservations []ports.Reservation
		Free         []int
		Total        int
	}{res.Reservations, res.Free, len(res.Free)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "ports.html", data); err != nil {
		s.logger.Error("ports: template", "err", err)
	}
}

func (s *Server) handleAPIRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.store.Config().Routes)
	case http.MethodPost:
		var body struct {
			Host        string `json:"host"`
			Upstream    string `json:"upstream"`
			Auth        string `json:"auth"`
			Health      string `json:"health"`
			StripPrefix string `json:"strip_prefix"`
			PathPrefix  string `json:"path_prefix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		idx, err := s.store.AddRoute(config.Route{
			Host:        body.Host,
			Upstream:    body.Upstream,
			Health:      body.Health,
			Auth:        body.Auth,
			StripPrefix: body.StripPrefix,
			PathPrefix:  body.PathPrefix,
		})
		_ = idx
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// auto-provision screenshot capture + reload
		go s.afterRouteAdded(body.Host)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(s.store.Config().Routes)
	case http.MethodDelete:
		host := r.URL.Query().Get("host")
		if host == "" {
			http.Error(w, "host query param required", http.StatusBadRequest)
			return
		}
		if err := s.store.RemoveRoute(host); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// afterRouteAdded reloads the proxy and triggers a screenshot capture.
// It also re-runs port scan to refresh the suggestions list.
func (s *Server) afterRouteAdded(host string) {
	if s.px != nil {
		_ = s.px.Reload()
	}
	if s.ss != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := s.ss.Capture(ctx, host); err != nil {
			s.logger.Warn("initial screenshot", "host", host, "err", err)
		}
	}
	go s.rescanPorts(context.Background())
}

func (s *Server) handleAPILog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.AccessLog(200))
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.Stats())
}

func (s *Server) handleAPIActiveConns(w http.ResponseWriter, r *http.Request) {
	all := s.px.AllStats()
	total := int64(0)
	for _, st := range all {
		total += st.ActiveConns
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total":    total,
		"per_host": all,
	})
}

func (s *Server) handleAPIRecapture(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ Host string `json:"host"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Host == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	if s.ss == nil {
		http.Error(w, "screenshot manager not configured", http.StatusServiceUnavailable)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := s.ss.Capture(ctx, body.Host); err != nil {
			s.logger.Warn("recapture", "host", body.Host, "err", err)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPIRecaptureAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.ss == nil {
		http.Error(w, "screenshot manager not configured", http.StatusServiceUnavailable)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.ss.Loop(ctx)
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPIRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	go s.rescanPorts(context.Background())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPISuggestions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.lastRescan)
}

// handleAPIPortsList returns the port-allocator state as JSON.
// Returns {reservations: [...], free: [...]}.
func (s *Server) handleAPIPortsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	res, err := s.ports.List(r.Context())
	if err != nil {
		http.Error(w, "allocator: "+err.Error(), 502)
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// handleAPIPortsClaim is POST. Body: {port, owner, note}. Returns
// the ClaimResult as JSON. Idempotent for the same owner.
func (s *Server) handleAPIPortsClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Port  int    `json:"port"`
		Owner string `json:"owner"`
		Note  string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), 400)
		return
	}
	if body.Port == 0 || body.Owner == "" {
		http.Error(w, "port and owner required", 400)
		return
	}
	res, err := s.ports.Claim(r.Context(), body.Port, body.Owner, body.Note)
	if err != nil {
		http.Error(w, "claim: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := 200
	if res.Status == "taken" {
		status = 409
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// handleAPIPortsRelease is POST. Body: {port, owner}. Returns 200
// on success, 403 if owner doesn't match, 404 if port not reserved.
func (s *Server) handleAPIPortsRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Port  int    `json:"port"`
		Owner string `json:"owner"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), 400)
		return
	}
	if err := s.ports.Release(r.Context(), body.Port, body.Owner); err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"status":"released","port":` + fmt.Sprint(body.Port) + `}`))
}

func (s *Server) handleAPIEnableFunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Host string `json:"host"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Funnel already runs on aigoproxy:80. We just need to add a Funnel
	// listener for the specific path. The simplest reliable command is:
	//   tailscale funnel --bg --set-path=<path> 80
	// (sets up path-based routing through the existing :80 listener).
	path := body.Path
	if path == "" {
		path = "/"
	}
	cmd := exec.Command("tailscale", "funnel", "--bg", "--yes", "--set-path", path, "80")
	out, err := cmd.CombinedOutput()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		s.logger.Warn("tailscale funnel", "err", err, "out", string(out))
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": string(out)})
		return
	}
	s.logger.Info("funnel enabled", "path", path, "out", string(out))
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": path})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if s.ss == nil {
		http.NotFound(w, r)
		return
	}
	host := strings.TrimPrefix(r.URL.Path, "/screenshots/")
	host = strings.TrimSuffix(host, ".png")
	if host == "" || strings.ContainsAny(host, "/\\") {
		http.NotFound(w, r)
		return
	}
	p := s.ss.PathFor(host)
	if p == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	http.ServeFile(w, r, p)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"now":      time.Now().Format(time.RFC3339),
		"data_dir": s.store.DataDir(),
		"stats":    s.store.Stats(),
	})
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "static fs error", http.StatusInternalServerError)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/static/")
	f, err := staticSub.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, _ := f.Stat()
	if stat == nil || stat.IsDir() {
		http.NotFound(w, r)
		return
	}
	// Set content type by extension
	switch filepath.Ext(p) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "static: not seekable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, p, stat.ModTime(), rs)
}

// Suggestions returns the most recent port-scan results. Exposed for
// the MCP server to use as aigoproxy_scan.
func (s *Server) Suggestions() []Suggestion {
	return s.lastRescan
}

// rescanPorts probes localhost on a list of well-known dev ports to
// suggest new services to register.
func (s *Server) rescanPorts(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("rescan panic", "err", rec)
		}
	}()
	// Common service ports on Manjaro dev / home-server setups
	ports := []int{
		80, 443, 3000, 3001, 4000, 4040, 5000, 5001, 5050, 5173, 6000, 6001,
		6379, 7000, 7474, 8000, 8001, 8080, 8081, 8082, 8083, 8086, 8088,
		8090, 8091, 8096, 8181, 8384, 8443, 8500, 8888, 8983, 8990, 9000, 9001,
		9090, 9091, 9200, 9443, 9999, 10000, 10110, 10250, 11211, 15672, 18080, 27017,
	}
	// Skip ports that are already configured
	cfg := s.store.Config()
	used := map[int]bool{}
	for _, r := range cfg.Routes {
		if u, err := urlPort(r.Upstream); err == nil {
			used[u] = true
		}
	}
	suggestions := []Suggestion{}
	for _, p := range ports {
		if used[p] {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		// Quick TCP probe
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		conn, err := dialTimeout(ctx, addr, 200*time.Millisecond)
		if err != nil {
			continue
		}
		conn.Close()
		// First byte guess: just use a common service name as fallback
		title := portGuess(p)
		first := firstByte(ctx, addr)
		// Inspect the upstream to see if it has its own auth.
		insp, _ := detector.Inspect(ctx, "http://"+addr)
		sg := Suggestion{
			Port:       p,
			TitleGuess: title,
			FirstByte:  first,
		}
		if insp != nil {
			sg.HasAuth = insp.HasAuth
			sg.AuthReason = insp.Reason
			sg.RecommendedAuth = insp.RecommendedAuth
		} else {
			sg.RecommendedAuth = "tailscale"
		}
		suggestions = append(suggestions, sg)
	}
	s.lastRescan = suggestions
	s.logger.Info("port rescan complete", "found", len(suggestions))
}

// portGuess returns a short name for a well-known port.
func portGuess(p int) string {
	known := map[int]string{
		80: "http", 443: "https", 3000: "node", 3001: "node2", 4000: "devserver",
		5000: "flask", 5001: "flask2", 5173: "vite", 6000: "x11", 6379: "redis",
		7000: "spring", 7474: "neo4j", 8000: "django", 8001: "alt", 8080: "tomcat",
		8081: "admin", 8082: "admin2", 8083: "admin3", 8086: "influxdb",
		8088: "radarr", 8090: "couchpotato", 8091: "freeipa", 8096: "jellyfin",
		8181: "http2", 8384: "syncthing", 8443: "https2", 8500: "consul",
		8888: "jupyter", 8983: "sonarr", 8990: "homeassistant", 9000: "portainer",
		9001: "traefik", 9090: "prometheus", 9091: "prom-federation", 9200: "elastic",
		9443: "cockpit", 9999: "adguard", 10000: "webmin", 10110: "rtorrent",
		10250: "kubelet", 11211: "memcached", 15672: "rabbitmq", 18080: "gitea",
		27017: "mongodb",
	}
	if v, ok := known[p]; ok {
		return v
	}
	return fmt.Sprintf("svc%d", p)
}

// dialTimeout is a non-blocking TCP probe.
func dialTimeout(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	// Use the package-level netDialer (overridable in tests).
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return netDialer.DialContext(dctx, "tcp", addr)
}

// firstByte sends an HTTP HEAD and returns the first line of the response
// (usually "HTTP/1.1 200 OK").
func firstByte(ctx context.Context, addr string) string {
	dctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := netDialer.DialContext(dctx, "tcp", addr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = conn.Write([]byte("GET / HTTP/1.0\r\nHost: localhost\r\n\r\n"))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if n == 0 {
		return ""
	}
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

func (s *Server) recoverMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic in handler", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (s *Server) logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rw, r)
		s.logger.Debug("webui",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
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
	return s.ResponseWriter.Write(b)
}
