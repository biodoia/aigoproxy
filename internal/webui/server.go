// Package webui serves the aigoproxy dashboard: list/edit routes,
// view access logs, see stats. Stdlib-only, anti-slop dark theme.
package webui

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the dashboard HTTP server.
type Server struct {
	addr   string
	store  *store.Store
	logger *slog.Logger
	tmpl   *template.Template
}

// New returns a new Server.
func New(addr string, s *store.Store, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	t, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		addr:   addr,
		store:  s,
		logger: logger,
		tmpl:   t,
	}, nil
}

// Handler returns the http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/routes", s.handleRoutes)
	mux.HandleFunc("/api/routes", s.handleAPIRoutes)
	mux.HandleFunc("/api/log", s.handleAPILog)
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/static/", s.handleStatic)
	return s.recoverMW(s.logMW(mux))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := struct {
		Title  string
		Stats  store.Stats
		Routes []routeView
	}{
		Title: "aigoproxy",
		Stats: s.store.Stats(),
		Routes: s.routesView(),
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

type routeView struct {
	Host     string
	Upstream string
	Auth     string
	Health   string
	Enabled  bool
}

func (s *Server) routesView() []routeView {
	cfg := s.store.Config()
	out := make([]routeView, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		out = append(out, routeView{
			Host: r.Host, Upstream: r.Upstream, Auth: r.Auth, Health: r.Health, Enabled: r.Enabled,
		})
	}
	return out
}

func (s *Server) handleAPIRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(s.store.Config().Routes)
	case http.MethodPost:
		// body: {host, upstream, auth, health, strip_prefix, path_prefix}
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

func (s *Server) handleAPILog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.AccessLog(200))
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.Stats())
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
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeContent(w, r, p, stat.ModTime(), f.(readSeeker))
}

type readSeeker interface {
	Read(p []byte) (n int, err error)
	Seek(offset int64, whence int) (int64, error)
}

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRW{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.status,
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRW struct {
	http.ResponseWriter
	status int
}

func (w *statusRW) WriteHeader(c int) {
	if w.status == 200 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusRW) Write(b []byte) (int, error) {
	return w.ResponseWriter.Write(b)
}

func (w *statusRW) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
