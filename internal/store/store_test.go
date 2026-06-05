package store

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
)

// testStore is a helper that returns a Store wired to a unique
// namespace so tests don't collide with each other or with the real
// `aigoproxy` production namespace. Each test gets a unique
// namespace (counter + random suffix) so we never have to clean
// memogo between runs.
func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	ns := fmt.Sprintf("aigoproxy_test_%d_%d", testStoreCounter.Add(1), rand.Int63())
	t.Setenv("AIGOPROXY_NAMESPACE", ns)
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, ns
}

var testStoreCounter atomic.Int64

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

// TestAddRouteValidatesHost ensures AddRoute refuses ghost routes
// (blank Host or Upstream) instead of polluting the live config.
func TestAddRouteValidatesHost(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRoute(config.Route{Host: "", Upstream: "http://127.0.0.1:9000"}); err == nil {
		t.Error("AddRoute with empty host should fail")
	}
	if _, err := s.AddRoute(config.Route{Host: "ok.test.ts.net", Upstream: ""}); err == nil {
		t.Error("AddRoute with empty upstream should fail")
	}
	if _, err := s.AddRoute(config.Route{Host: "  ", Upstream: "http://127.0.0.1:9000"}); err == nil {
		t.Error("AddRoute with whitespace host should fail")
	}
	if _, err := s.AddRoute(config.Route{Host: "real.test.ts.net", Upstream: "http://127.0.0.1:9000"}); err != nil {
		t.Errorf("AddRoute valid: %v", err)
	}
}

// TestPruneInvalidRoutes verifies the prune helper cleans up ghost
// routes that snuck into the config through some legacy path.
func TestPruneInvalidRoutes(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	// Seed a ghost route by writing directly to memogo with blank host.
	// (The AddRoute validation prevents it going forward, but legacy
	// state from before the fix may still have it.)
	ghostBody, _ := json.Marshal(config.Route{Host: "", Upstream: ""})
	if err := s.MemogoStoreForTest("route:", ghostBody, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRoute(config.Route{Host: "keeper.test.ts.net", Upstream: "http://127.0.0.1:9000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	// Pre-prune: should have at least 1 ghost (the one we just wrote).
	hasGhost := false
	for _, r := range s.Config().Routes {
		if r.Host == "" {
			hasGhost = true
		}
	}
	if !hasGhost {
		t.Fatal("setup: expected a ghost route to be present before prune")
	}
	n, err := s.PruneInvalidRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("expected prune >= 1, got %d", n)
	}
	for _, r := range s.Config().Routes {
		if r.Host == "" {
			t.Errorf("ghost route survived prune: %+v", r)
		}
	}
}

// TestTombstoneDataOnly verifies the load-time skip survives even
// when the tombstone marker is only in the JSON body (i.e. memogo
// has already dropped the custom __tombstone__ meta key). This is
// the realistic state of aigoproxy running against memogo v2 today:
// custom meta is silently stripped on round-trip, so the data-only
// path is the one that actually fires in production.
func TestTombstoneDataOnly(t *testing.T) {
	s, _ := testStore(t)
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRoute(config.Route{Host: "victim.test.ts.net", Upstream: "http://127.0.0.1:9000", Auth: "none"}); err != nil {
		t.Fatal(err)
	}
	// Confirm the route is live.
	found := false
	for _, r := range s.Config().Routes {
		if r.Host == "victim.test.ts.net" {
			found = true
		}
	}
	if !found {
		t.Fatal("victim route missing before tombstone")
	}
	// Now remove it — RemoveRouteWithActor writes the tombstone
	// marker to BOTH meta and body. The fake preserves meta, so
	// this also exercises the meta path.
	if err := s.RemoveRouteWithActor("victim.test.ts.net", "test", "data-only-tombstone-test"); err != nil {
		t.Fatal(err)
	}
	// Manually overwrite the entry with an empty meta to mimic
	// memogo v2's round-trip. We poke through the in-process store
	// API by re-storing with the tombstone body but no custom meta.
	body, _ := json.Marshal(tombstoneData{
		Tombstone:    "1",
		RemovedBy:    "test",
		RemovedAt:    time.Now().UTC().Format(time.RFC3339),
		OriginalHost: "victim.test.ts.net",
	})
	if err := s.MemogoStoreForTest("route:victim.test.ts.net", body, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	// Reload and verify the route is gone.
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Config().Routes {
		if r.Host == "victim.test.ts.net" {
			t.Errorf("tombstoned route still present in cfg: %+v", r)
		}
	}
	// TombstoneSummary should still find it via the body path.
	n, err := s.TombstoneSummary()
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("expected TombstoneSummary >= 1, got %d", n)
	}
}
