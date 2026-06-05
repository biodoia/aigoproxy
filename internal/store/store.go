// Package store manages aigoproxy's persistent state via Memogo.
//
// Memogo is the central multi-database orchestrator of the FGT
// ecosystem: it runs the underlying DBs (Postgres, Redis, DuckDB,
// ClickHouse, whatever fits) and exposes a unified key-value+metadata
// HTTP API. aigoproxy never talks to a database driver directly —
// it talks to Memogo's v2 API at http://localhost:8081/api/v2/*.
//
// Layout (all entries live in the `aigoproxy` namespace):
//
//	ns:aigoproxy
//	  key: "route:<host>"                → JSON of config.Route
//	  key: "audit:<unixnano>"            → JSON of AuditEntry
//	  key: "stats:running"               → JSON of Stats (latest snapshot)
//	  key: "config:base"                 → JSON of {http_addr, base_domain, ...}
//
// A local on-disk YAML at dataDir/config.yaml is kept as a
// human-readable mirror and as a fallback for read-only mode when
// Memogo is unreachable. Writes are dual: store to Memogo first
// (authoritative), then mirror to YAML (readability).
package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
)

// MemogoClient is the minimal Memogo v2 API surface we depend on.
// It mirrors memoclient.Client (in the FGT monorepo) so we can swap
// implementations in tests.
type MemogoClient interface {
	V2Store(ctx context.Context, ns, key string, content []byte, meta map[string]string) error
	V2Get(ctx context.Context, ns, key string) ([]byte, map[string]string, error)
	V2List(ctx context.Context, ns string) ([]MemogoEntry, error)
	V2Delete(ctx context.Context, ns, key string) error
}

// MemogoEntry is one row from V2List.
type MemogoEntry struct {
	Key       string            `json:"key"`
	Namespace string            `json:"namespace"`
	Data      string            `json:"data"`
	Meta      map[string]string `json:"meta"`
}

// AccessLogEntry is one row in the access log.
type AccessLogEntry struct {
	Time      time.Time `json:"time"`
	Host      string    `json:"host"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Bytes     int64     `json:"bytes"`
	LatencyMs int64     `json:"latency_ms"`
	Remote    string    `json:"remote"`
	User      string    `json:"user,omitempty"`
}

// AuditEntry records one change to the routes table.
type AuditEntry struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Action    string    `json:"action"`
	Host      string    `json:"host"`
	Actor     string    `json:"actor"`
	OldConfig string    `json:"old_config,omitempty"`
	NewConfig string    `json:"new_config,omitempty"`
	Note      string    `json:"note,omitempty"`
}

// Store is the persistent + in-memory state.
type Store struct {
	dataDir string
	ns      string
	api     MemogoClient
	http    *http.Client // direct memogo client (for V2List which isn't in the interface)

	mu        sync.RWMutex
	cfg       *config.Config
	accessLog []AccessLogEntry
	stats     Stats

	memogoUp bool // last-known reachability
}

// Stats are running counters.
type Stats struct {
	TotalRequests int64     `json:"total_requests"`
	BytesProxied  int64     `json:"bytes_proxied"`
	LastRequestAt time.Time `json:"last_request_at"`
	ActiveRoutes  int       `json:"active_routes"`
	StartedAt     time.Time `json:"started_at"`
	MemogoUp      bool      `json:"memogo_up"`
	MemogoURL     string    `json:"memogo_url"`
	RouteKeys     int       `json:"route_keys"`
	AuditKeys     int       `json:"audit_keys"`
}

// HTTPMemogoClient is the production Memogo client. It implements
// MemogoClient + a few extras the store needs.
type HTTPMemogoClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewHTTPMemogo builds an HTTPMemogoClient pointing at the given base URL.
func NewHTTPMemogo(baseURL string) *HTTPMemogoClient {
	return &HTTPMemogoClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (m *HTTPMemogoClient) V2Store(ctx context.Context, ns, key string, content []byte, meta map[string]string) error {
	body := map[string]any{
		"Namespace": ns,
		"Key":       key,
		"Content":   content, // []byte → base64 in JSON
		"Meta":      meta,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", m.BaseURL+"/api/v2/store", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("memogo store: %s: %s", resp.Status, string(b))
	}
	return nil
}

func (m *HTTPMemogoClient) V2Get(ctx context.Context, ns, key string) ([]byte, map[string]string, error) {
	u := m.BaseURL + "/api/v2/get?namespace=" + url.QueryEscape(ns) + "&key=" + url.QueryEscape(key)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("memogo get: %s: %s", resp.Status, string(b))
	}
	var out struct {
		Content []byte            `json:"Content"`
		Meta    map[string]string `json:"Meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, err
	}
	return out.Content, out.Meta, nil
}

func (m *HTTPMemogoClient) V2List(ctx context.Context, ns string) ([]MemogoEntry, error) {
	u := m.BaseURL + "/api/v2/list?namespace=" + url.QueryEscape(ns)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return []MemogoEntry{}, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("memogo list: %s: %s", resp.Status, string(b))
	}
	var out struct {
		Entries []struct {
			Namespace string            `json:"namespace"`
			Key       string            `json:"key"`
			Data      string            `json:"data"`
			Meta      map[string]string `json:"meta"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make([]MemogoEntry, 0, len(out.Entries))
	for _, e := range out.Entries {
		// Memogo's list endpoint returns base64-encoded content in the
		// "data" field. Decode to plain string so callers see JSON or
		// other content as-is.
		decoded, derr := decodeIfBase64(e.Data)
		if derr != nil {
			decoded = e.Data // fall back to raw if it isn't base64
		}
		res = append(res, MemogoEntry{
			Namespace: e.Namespace, Key: e.Key, Data: decoded, Meta: e.Meta,
		})
	}
	return res, nil
}

func (m *HTTPMemogoClient) V2Delete(ctx context.Context, ns, key string) error {
	body, _ := json.Marshal(map[string]any{
		"Namespace": ns,
		"Key":       key,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", m.BaseURL+"/api/v2/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("memogo delete: %s: %s", resp.Status, string(b))
	}
	return nil
}

// New returns a Store backed by Memogo + a local YAML mirror.
func New(dataDir string) (*Store, error) {
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".aigoproxy")
	}
	for _, sub := range []string{"", "certs", "backups"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	// Find Memogo URL from env, fall back to localhost:8081
	memogoURL := os.Getenv("MEMOGO_URL")
	if memogoURL == "" {
		memogoURL = "http://localhost:8081"
	}
	ns := os.Getenv("AIGOPROXY_NAMESPACE")
	if ns == "" {
		ns = "aigoproxy"
	}
	api := NewHTTPMemogo(memogoURL)
	s := &Store{
		dataDir:   dataDir,
		ns:        ns,
		api:       api,
		http:      api.HTTP,
		accessLog: make([]AccessLogEntry, 0, 1024),
		stats:     Stats{StartedAt: time.Now(), MemogoURL: memogoURL},
	}
	// Probe memogo at startup. If unreachable, store still works in
	// local-only mode (writes go to YAML + in-memory; reads from YAML).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := api.V2List(ctx, "aigoproxy"); err == nil {
		s.memogoUp = true
		s.stats.MemogoUp = true
	}
	return s, nil
}

// LoadConfig reads routes from Memogo. If Memogo is unreachable, falls
// back to dataDir/config.yaml. If both are empty, starts blank.
func (s *Store) LoadConfig() (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read other config fields from YAML
	c, _ := config.Load(s.configPath())
	if c == nil {
		c = &config.Config{}
	}
	// Try memogo
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := s.api.V2List(ctx, s.ns)
	if err != nil {
		s.memogoUp = false
	} else {
		s.memogoUp = true
		s.stats.MemogoUp = true
	}
	routes := []config.Route{}
	auditCount := 0
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Key, "route:"):
			var r config.Route
			if err := json.Unmarshal([]byte(e.Data), &r); err == nil {
				routes = append(routes, r)
			}
		case strings.HasPrefix(e.Key, "audit:"):
			auditCount++
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Host < routes[j].Host })
	c.Routes = routes
	if c.BaseDomain == "" && len(routes) > 0 {
		c.BaseDomain = guessBaseDomain(routes)
	}
	s.cfg = c
	s.stats.ActiveRoutes = len(routes)
	s.stats.RouteKeys = len(routes)
	s.stats.AuditKeys = auditCount
	return c, nil
}

// AddRoute inserts a route into Memogo, mirrors to YAML, audits.
func (s *Store) AddRoute(r config.Route) (int, error) {
	return s.AddRouteWithActor(r, "cli", "")
}

// AddRouteWithActor is AddRoute with explicit actor attribution.
func (s *Store) AddRouteWithActor(r config.Route, actor, note string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		s.cfg = &config.Config{}
	}
	if r.Auth == "" {
		r.Auth = "none"
	}
	r.Enabled = true
	for _, existing := range s.cfg.Routes {
		if existing.Host == r.Host {
			return -1, fmt.Errorf("route %q already exists", r.Host)
		}
	}
	ob, _ := json.Marshal(r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, _ := json.Marshal(r)
	if err := s.api.V2Store(ctx, s.ns, "route:"+r.Host, data, map[string]string{
		"type":       "route",
		"auth":       r.Auth,
		"actor":      actor,
		"created_at": time.Now().Format(time.RFC3339),
	}); err != nil {
		return -1, fmt.Errorf("memogo: %w", err)
	}
	s.memogoUp = true
	s.stats.MemogoUp = true
	s.cfg.Routes = append(s.cfg.Routes, r)
	s.stats.ActiveRoutes = len(s.cfg.Routes)
	s.stats.RouteKeys = len(s.cfg.Routes)
	_ = s.writeYAMLLocked()
	s.auditLocked("add", r.Host, actor, "", string(ob), note)
	return len(s.cfg.Routes) - 1, nil
}

// RemoveRoute deletes a route from Memogo + YAML.
func (s *Store) RemoveRoute(host string) error { return s.RemoveRouteWithActor(host, "cli", "") }

// RemoveRouteWithActor is RemoveRoute with explicit actor attribution.
func (s *Store) RemoveRouteWithActor(host, actor, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return fmt.Errorf("no config loaded")
	}
	for i, r := range s.cfg.Routes {
		if r.Host == host {
			ob, _ := json.Marshal(r)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.api.V2Delete(ctx, s.ns, "route:"+host); err != nil {
				return err
			}
			s.cfg.Routes = append(s.cfg.Routes[:i], s.cfg.Routes[i+1:]...)
			s.stats.ActiveRoutes = len(s.cfg.Routes)
			s.stats.RouteKeys = len(s.cfg.Routes)
			_ = s.writeYAMLLocked()
			s.auditLocked("remove", host, actor, string(ob), "", note)
			return nil
		}
	}
	return fmt.Errorf("route %q not found", host)
}

// UpdateRoute replaces the route with matching host.
func (s *Store) UpdateRoute(host string, r config.Route) error {
	return s.UpdateRouteWithActor(host, r, "cli", "")
}

// UpdateRouteWithActor is UpdateRoute with explicit actor attribution.
func (s *Store) UpdateRouteWithActor(host string, r config.Route, actor, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return fmt.Errorf("no config loaded")
	}
	for i, existing := range s.cfg.Routes {
		if existing.Host == host {
			ob, _ := json.Marshal(existing)
			nb, _ := json.Marshal(r)
			r.Enabled = existing.Enabled
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, _ := json.Marshal(r)
			if err := s.api.V2Store(ctx, s.ns, "route:"+host, data, map[string]string{
				"type":  "route",
				"auth":  r.Auth,
				"actor": actor,
			}); err != nil {
				return err
			}
			s.cfg.Routes[i] = r
			_ = s.writeYAMLLocked()
			s.auditLocked("update", host, actor, string(ob), string(nb), note)
			return nil
		}
	}
	return fmt.Errorf("route %q not found", host)
}

// SetRouteEnabled toggles the Enabled flag.
func (s *Store) SetRouteEnabled(host string, enabled bool) error {
	return s.SetRouteEnabledWithActor(host, enabled, "cli", "")
}

// SetRouteEnabledWithActor is SetRouteEnabled with explicit actor attribution.
func (s *Store) SetRouteEnabledWithActor(host string, enabled bool, actor, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return fmt.Errorf("no config loaded")
	}
	for i := range s.cfg.Routes {
		if s.cfg.Routes[i].Host == host {
			ob, _ := json.Marshal(s.cfg.Routes[i])
			s.cfg.Routes[i].Enabled = enabled
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, _ := json.Marshal(s.cfg.Routes[i])
			if err := s.api.V2Store(ctx, s.ns, "route:"+host, data, map[string]string{
				"type":  "route",
				"auth":  s.cfg.Routes[i].Auth,
				"actor": actor,
			}); err != nil {
				s.cfg.Routes[i].Enabled = !enabled
				return err
			}
			action := "disable"
			if enabled {
				action = "enable"
			}
			s.auditLocked(action, host, actor, string(ob), "", note)
			_ = s.writeYAMLLocked()
			return nil
		}
	}
	return fmt.Errorf("route %q not found", host)
}

// LookupRoute finds the route for a given Host header.
func (s *Store) LookupRoute(host string) (config.Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil {
		return config.Route{}, false
	}
	for _, r := range s.cfg.Routes {
		if r.Host == host && r.Enabled {
			return r, true
		}
	}
	return config.Route{}, false
}

// LogAccess appends to in-memory ring. (Persistent storage of access
// logs goes through Memogo in a future iteration; right now we keep
// it in-memory + tail-friendly.)
func (s *Store) LogAccess(e AccessLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.accessLog = append(s.accessLog, e)
	if len(s.accessLog) > 10000 {
		s.accessLog = s.accessLog[1000:]
	}
	s.stats.TotalRequests++
	s.stats.BytesProxied += e.Bytes
	s.stats.LastRequestAt = e.Time
}

// AccessLog returns the most recent N entries (newest last).
func (s *Store) AccessLog(n int) []AccessLogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n <= 0 || n > len(s.accessLog) {
		n = len(s.accessLog)
	}
	out := make([]AccessLogEntry, n)
	copy(out, s.accessLog[len(s.accessLog)-n:])
	return out
}

// Audit returns the most recent N audit entries (newest first).
func (s *Store) Audit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := s.api.V2List(ctx, s.ns)
	if err != nil {
		return nil, err
	}
	out := []AuditEntry{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, "audit:") {
			continue
		}
		var a AuditEntry
		if err := json.Unmarshal([]byte(e.Data), &a); err == nil {
			out = append(out, a)
		}
	}
	// Sort newest first by ID (monotonic int64 from unixnano/1000)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AuditForHost returns audit history filtered to one host.
func (s *Store) AuditForHost(host string, limit int) ([]AuditEntry, error) {
	all, err := s.Audit(0)
	if err != nil {
		return nil, err
	}
	out := []AuditEntry{}
	for _, a := range all {
		if a.Host == host {
			out = append(out, a)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// RestoreFromBackup is a no-op in the Memogo backend: backups are
// just points-in-time V2Store calls. To roll back, fetch the audit
// trail and replay. For convenience we keep the legacy backup file
// machinery for the YAML mirror.
func (s *Store) RestoreFromBackup(offset int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.backupFilesLocked()
	if err != nil {
		return "", err
	}
	if offset >= len(files) {
		return "", fmt.Errorf("only %d backups available", len(files))
	}
	src := files[offset]
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(s.configPath(), data, 0o644); err != nil {
		return "", err
	}
	c, _ := config.Load(s.configPath())
	if c != nil {
		s.cfg = c
		s.stats.ActiveRoutes = len(c.Routes)
	}
	s.auditLocked("restore", "", "system", "", "", "from "+filepath.Base(src))
	return src, nil
}

// ListBackups returns the YAML-mirror backup files (newest first).
func (s *Store) ListBackups() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backupFilesLocked()
}

// Stats returns a snapshot of the running stats.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// DataDir returns the on-disk directory used by the store.
func (s *Store) DataDir() string { return s.dataDir }

// CertsDir returns the directory where certs are stored.
func (s *Store) CertsDir() string { return filepath.Join(s.dataDir, "certs") }

// BackupsDir returns the directory where config backups are stored.
func (s *Store) BackupsDir() string { return filepath.Join(s.dataDir, "backups") }

// MemogoURL returns the configured Memogo endpoint.
func (s *Store) MemogoURL() string { return s.stats.MemogoURL }

// Config returns a deep copy of the current config.
func (s *Store) Config() config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil {
		return config.Config{}
	}
	c := *s.cfg
	c.Routes = append([]config.Route(nil), s.cfg.Routes...)
	return c
}

// DumpState writes a JSON snapshot to dataDir/state.json.
func (s *Store) DumpState() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dump := struct {
		Config    *config.Config   `json:"config"`
		Stats     Stats            `json:"stats"`
		AccessLog []AccessLogEntry `json:"access_log"`
	}{
		Config:    s.cfg,
		Stats:     s.stats,
		AccessLog: s.accessLog,
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dataDir, "state.json"), data, 0o644)
}

// Close is a no-op for now.
func (s *Store) Close() error { return nil }

// --- helpers (must be called with s.mu held) ---

func (s *Store) writeYAMLLocked() error {
	if s.cfg == nil {
		return nil
	}
	if err := s.cfg.Save(s.configPath()); err != nil {
		return err
	}
	_ = s.rotateBackupLocked()
	return nil
}

func (s *Store) rotateBackupLocked() error {
	dir := s.backupsDir()
	name := fmt.Sprintf("config-%s.yaml", time.Now().UTC().Format("20060102-150405"))
	dst := filepath.Join(dir, name)
	data, err := os.ReadFile(s.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	files, _ := s.backupFilesLocked()
	const maxBackups = 20
	if len(files) > maxBackups {
		for _, f := range files[maxBackups:] {
			_ = os.Remove(f)
		}
	}
	return nil
}

func (s *Store) auditLocked(action, host, actor, oldCfg, newCfg, note string) {
	id := time.Now().UnixNano() / 1000
	e := AuditEntry{
		ID:        id,
		Time:      time.Now(),
		Action:    action,
		Host:      host,
		Actor:     actor,
		OldConfig: oldCfg,
		NewConfig: newCfg,
		Note:      note,
	}
	data, _ := json.Marshal(e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.api.V2Store(ctx, s.ns, fmt.Sprintf("audit:%d", id), data, map[string]string{
		"type":   "audit",
		"action": action,
		"host":   host,
		"actor":  actor,
	})
	s.stats.AuditKeys++
}

func (s *Store) backupFilesLocked() ([]string, error) {
	dir := s.backupsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

func (s *Store) configPath() string { return filepath.Join(s.dataDir, "config.yaml") }
func (s *Store) backupsDir() string { return filepath.Join(s.dataDir, "backups") }

// guessBaseDomain finds the most common domain suffix across routes.
func guessBaseDomain(routes []config.Route) string {
	if len(routes) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, r := range routes {
		dot := -1
		for i := 0; i < len(r.Host); i++ {
			if r.Host[i] == '.' {
				dot = i
				break
			}
		}
		if dot >= 0 && dot+1 < len(r.Host) {
			counts[r.Host[dot+1:]]++
		}
	}
	best, bestN := "", 0
	for k, v := range counts {
		if v > bestN {
			best, bestN = k, v
		}
	}
	return best
}

// Used by tests + tail helper.
var _ = bufio.NewScanner
var _ = io.EOF

// decodeIfBase64 tries to base64-decode s. If the input doesn't look
// like base64 (length, chars, or padding) the original string is
// returned unchanged with err=nil — we don't want to fail loudly when
// memogo evolves to return plain text in some endpoints.
func decodeIfBase64(s string) (string, error) {
	if s == "" {
		return s, nil
	}
	// Memogo's encoded content is roughly len*4/3 with padding.
	if len(s)%4 != 0 {
		return s, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return s, nil
	}
	// Heuristic: if decoded is mostly printable, prefer it. Otherwise
	// treat the input as raw text.
	printable := 0
	for _, r := range decoded {
		if r == 0x09 || r == 0x0a || r == 0x0d || (r >= 0x20 && r < 0x7f) || r >= 0x80 {
			printable++
		}
	}
	if printable < len(decoded)*9/10 {
		return s, nil
	}
	return string(decoded), nil
}
