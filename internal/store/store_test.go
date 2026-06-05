package store

import (
	"path/filepath"
	"testing"

	"github.com/biodoia/aigoproxy/internal/config"
)

// testStore is a helper that returns a Store wired to a unique
// namespace so tests don't collide with each other or with the real
// `aigoproxy` production namespace.
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	// Set env so the production New() picks the test namespace. We
	// use a per-test unique suffix.
	ns := "aigoproxy_test_" + filepath.Base(dir)
	t.Setenv("AIGOPROXY_NAMESPACE", ns)
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, ns
}

func TestNew(t *testing.T) {
	s, _ := testStore(t)
	if s.DataDir() == "" {
		t.Error("DataDir empty")
	}
}

func TestAddRemoveRoute(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.LoadConfig(); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if _, err := s.AddRoute(config.Route{Host: "a.test.ts.net", Upstream: "http://1.1.1.1:80"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if _, err := s.AddRoute(config.Route{Host: "b.test.ts.net", Upstream: "http://1.1.1.1:81"}); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	if len(s.Config().Routes) != 2 {
		t.Errorf("got %d routes, want 2", len(s.Config().Routes))
	}

	if err := s.RemoveRoute("a.test.ts.net"); err != nil {
		t.Fatalf("RemoveRoute: %v", err)
	}
	if len(s.Config().Routes) != 1 {
		t.Errorf("got %d routes after remove, want 1", len(s.Config().Routes))
	}
}

func TestAddDuplicateRoute(t *testing.T) {
	s, _ := testStore(t)
	s.LoadConfig()
	_, _ = s.AddRoute(config.Route{Host: "a.test.ts.net", Upstream: "http://1.1.1.1:80"})
	_, err := s.AddRoute(config.Route{Host: "a.test.ts.net", Upstream: "http://1.1.1.1:81"})
	if err == nil {
		t.Error("expected error for duplicate host")
	}
}

func TestLookupRoute(t *testing.T) {
	s, _ := testStore(t)
	s.LoadConfig()
	_, _ = s.AddRoute(config.Route{Host: "a.test.ts.net", Upstream: "http://1.1.1.1:80"})
	_, _ = s.AddRoute(config.Route{Host: "b.test.ts.net", Upstream: "http://1.1.1.1:81"})

	r, ok := s.LookupRoute("a.test.ts.net")
	if !ok {
		t.Fatal("expected to find a.test.ts.net")
	}
	if r.Upstream != "http://1.1.1.1:80" {
		t.Errorf("upstream = %q, want http://1.1.1.1:80", r.Upstream)
	}

	_, ok = s.LookupRoute("nonexistent.test.ts.net")
	if ok {
		t.Error("did not expect to find nonexistent host")
	}
}

func TestLogAccess(t *testing.T) {
	s, _ := testStore(t)
	s.LogAccess(AccessLogEntry{Host: "x", Method: "GET", Path: "/", Status: 200, LatencyMs: 5})
	s.LogAccess(AccessLogEntry{Host: "y", Method: "GET", Path: "/", Status: 500, LatencyMs: 100})
	if got := len(s.AccessLog(10)); got != 2 {
		t.Errorf("got %d log entries, want 2", got)
	}
	stats := s.Stats()
	if stats.TotalRequests != 2 {
		t.Errorf("total requests = %d, want 2", stats.TotalRequests)
	}
}

func TestStats(t *testing.T) {
	s, _ := testStore(t)
	stats := s.Stats()
	if stats.StartedAt.IsZero() {
		t.Error("StartedAt should be set")
	}
	if stats.ActiveRoutes != 0 {
		t.Errorf("ActiveRoutes = %d, want 0", stats.ActiveRoutes)
	}
}

func TestSetRouteEnabled(t *testing.T) {
	s, _ := testStore(t)
	s.LoadConfig()
	_, _ = s.AddRoute(config.Route{Host: "a.test.ts.net", Upstream: "http://1.1.1.1:80"})
	if err := s.SetRouteEnabled("a.test.ts.net", false); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	if cfg.Routes[0].Enabled {
		t.Error("expected Enabled=false")
	}
	// lookup should fail when disabled
	if _, ok := s.LookupRoute("a.test.ts.net"); ok {
		t.Error("disabled route should not be lookup-able")
	}
}

func TestRemoveMissing(t *testing.T) {
	s, _ := testStore(t)
	s.LoadConfig()
	if err := s.RemoveRoute("nope.test.ts.net"); err == nil {
		t.Error("expected error removing missing route")
	}
}
