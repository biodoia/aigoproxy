// Package store manages the in-memory + on-disk state of aigoproxy.
// State includes routes, cert metadata, and access logs.
//
// Concurrency: Store is safe for concurrent use. All mutating operations
// take the write lock; reads take the read lock.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
)

// AccessLogEntry is one row in the access log.
type AccessLogEntry struct {
	Time     time.Time `json:"time"`
	Host     string    `json:"host"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Status   int       `json:"status"`
	Bytes    int64     `json:"bytes"`
	LatencyMs int64    `json:"latency_ms"`
	Remote   string    `json:"remote"`
	User     string    `json:"user,omitempty"`
}

// Store is the persistent + in-memory state.
type Store struct {
	dataDir string

	mu         sync.RWMutex
	cfg        *config.Config
	accessLog  []AccessLogEntry
	stats      Stats
}

// Stats are running counters.
type Stats struct {
	TotalRequests   int64     `json:"total_requests"`
	BytesProxied    int64     `json:"bytes_proxied"`
	LastRequestAt   time.Time `json:"last_request_at"`
	ActiveRoutes    int       `json:"active_routes"`
	StartedAt       time.Time `json:"started_at"`
}

// New returns a Store backed by dataDir. If dataDir is empty, it uses
// ~/.aigoproxy. Creates the directory tree if missing.
func New(dataDir string) (*Store, error) {
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".aigoproxy")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "certs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir certs: %w", err)
	}
	return &Store{
		dataDir:   dataDir,
		accessLog: make([]AccessLogEntry, 0, 1024),
		stats:     Stats{StartedAt: time.Now()},
	}, nil
}

// LoadConfig loads (or initializes) the config from dataDir/config.yaml.
func (s *Store) LoadConfig() (*config.Config, error) {
	path := s.configPath()
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
	return c, nil
}

// SaveConfig persists the in-memory config to disk.
func (s *Store) SaveConfig() error {
	s.mu.RLock()
	c := s.cfg
	s.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("no config loaded")
	}
	return c.Save(s.configPath())
}

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

// AddRoute inserts a route, persists, and returns the index.
func (s *Store) AddRoute(r config.Route) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.cfg.Routes {
		if existing.Host == r.Host {
			return -1, fmt.Errorf("route %q already exists", r.Host)
		}
	}
	if r.Auth == "" {
		r.Auth = "none"
	}
	r.Enabled = true
	s.cfg.Routes = append(s.cfg.Routes, r)
	idx := len(s.cfg.Routes) - 1
	s.stats.ActiveRoutes = len(s.cfg.Routes)
	return idx, s.persistLocked()
}

// RemoveRoute deletes the route with the given host.
func (s *Store) RemoveRoute(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.cfg.Routes {
		if r.Host == host {
			s.cfg.Routes = append(s.cfg.Routes[:i], s.cfg.Routes[i+1:]...)
			s.stats.ActiveRoutes = len(s.cfg.Routes)
			return s.persistLocked()
		}
	}
	return fmt.Errorf("route %q not found", host)
}

// UpdateRoute replaces the route with matching host. Returns error if absent.
func (s *Store) UpdateRoute(host string, r config.Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.cfg.Routes {
		if existing.Host == host {
			r.Enabled = existing.Enabled
			s.cfg.Routes[i] = r
			return s.persistLocked()
		}
	}
	return fmt.Errorf("route %q not found", host)
}

// SetRouteEnabled toggles the Enabled flag without removing the route.
func (s *Store) SetRouteEnabled(host string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Routes {
		if s.cfg.Routes[i].Host == host {
			s.cfg.Routes[i].Enabled = enabled
			return s.persistLocked()
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

// LogAccess appends an entry to the access log, capping at 10k entries.
func (s *Store) LogAccess(e AccessLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.accessLog = append(s.accessLog, e)
	if len(s.accessLog) > 10000 {
		// drop oldest 1000
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

// DumpState writes a JSON snapshot of the store to dataDir/state.json.
// Useful for debugging.
func (s *Store) DumpState() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dump := struct {
		Config    *config.Config     `json:"config"`
		Stats     Stats              `json:"stats"`
		AccessLog []AccessLogEntry   `json:"access_log"`
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

func (s *Store) persistLocked() error {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.Save(s.configPath())
}

func (s *Store) configPath() string {
	return filepath.Join(s.dataDir, "config.yaml")
}
