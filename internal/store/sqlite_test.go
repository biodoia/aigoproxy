// Tests for the Memogo-backed store. Uses an in-memory Memogo
// fake so we don't depend on a real memogo instance.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
)

// fakeMemogo is an in-memory implementation of MemogoClient.
type fakeMemogo struct {
	mu     sync.Mutex
	data   map[string]map[string]fakeEntry // ns → key → entry
	down   bool
}

type fakeEntry struct {
	Data string
	Meta map[string]string
}

func newFakeMemogo() *fakeMemogo {
	return &fakeMemogo{data: map[string]map[string]fakeEntry{}}
}

func (f *fakeMemogo) V2Store(ctx context.Context, ns, key string, content []byte, meta map[string]string) error {
	if f.down {
		return errors.New("memogo down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data[ns] == nil {
		f.data[ns] = map[string]fakeEntry{}
	}
	f.data[ns][key] = fakeEntry{Data: string(content), Meta: meta}
	return nil
}

func (f *fakeMemogo) V2Get(ctx context.Context, ns, key string) ([]byte, map[string]string, error) {
	if f.down {
		return nil, nil, errors.New("memogo down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.data[ns][key]; ok {
		return []byte(e.Data), e.Meta, nil
	}
	return nil, nil, nil
}

func (f *fakeMemogo) V2List(ctx context.Context, ns string) ([]MemogoEntry, error) {
	if f.down {
		return nil, errors.New("memogo down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res := []MemogoEntry{}
	for k, e := range f.data[ns] {
		res = append(res, MemogoEntry{
			Namespace: ns, Key: k, Data: e.Data, Meta: e.Meta,
		})
	}
	return res, nil
}

func (f *fakeMemogo) V2Delete(ctx context.Context, ns, key string) error {
	if f.down {
		return errors.New("memogo down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data[ns] != nil {
		delete(f.data[ns], key)
	}
	return nil
}

// newTestStore returns a Store wired to a fake Memogo. Skips the
// initial Memogo probe by using a data dir that doesn't exist yet.
func newTestStore(t *testing.T) (*Store, *fakeMemogo) {
	t.Helper()
	dir := t.TempDir()
	mem := newFakeMemogo()
	s := &Store{
		dataDir:   dir,
		ns:        "aigoproxy",
		api:       mem,
		http:      nil,
		accessLog: []AccessLogEntry{},
		stats:     Stats{StartedAt: time.Now(), MemogoUp: true, MemogoURL: "fake://"},
		memogoUp:  true,
	}
	// Seed one route via the API directly so LoadConfig has something
	// to find.
	_ = mem.V2Store(context.Background(), "aigoproxy", "route:hello.test", []byte(`{"Host":"hello.test","Upstream":"http://127.0.0.1:1","Auth":"tailscale","Enabled":true}`), map[string]string{"type": "route"})
	return s, mem
}

func TestAddRoutePersists(t *testing.T) {
	s, mem := newTestStore(t)
	// LoadConfig picks up the seeded route + reads new ones
	_, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := s.AddRouteWithActor(config.Route{
		Host: "added.test", Upstream: "http://127.0.0.1:2", Auth: "funnel",
	}, "mcp", "agent added")
	if err != nil {
		t.Fatal(err)
	}
	if idx < 0 {
		t.Fatal("expected idx >= 0")
	}
	// Confirm memogo has the entry
	list, _ := mem.V2List(context.Background(), "aigoproxy")
	found := false
	for _, e := range list {
		if e.Key == "route:added.test" {
			found = true
			var r config.Route
			_ = json.Unmarshal([]byte(e.Data), &r)
			if r.Upstream != "http://127.0.0.1:2" {
				t.Errorf("upstream lost: %q", r.Upstream)
			}
		}
	}
	if !found {
		t.Error("route not in memogo")
	}
	// Audit entry should exist
	audit, _ := s.Audit(0)
	if len(audit) < 1 {
		t.Error("no audit entry")
	} else if audit[0].Action != "add" || audit[0].Host != "added.test" || audit[0].Actor != "mcp" {
		t.Errorf("audit wrong: %+v", audit[0])
	}
}

func TestRemoveRoute(t *testing.T) {
	s, _ := newTestStore(t)
	_, _ = s.LoadConfig()
	if err := s.RemoveRouteWithActor("hello.test", "cli", ""); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	for _, r := range cfg.Routes {
		if r.Host == "hello.test" {
			t.Error("route still present after remove")
		}
	}
}

func TestMemogoDownFallback(t *testing.T) {
	s, mem := newTestStore(t)
	_, _ = s.LoadConfig()
	// Take memogo offline
	mem.down = true
	_, err := s.AddRouteWithActor(config.Route{Host: "offline.test", Upstream: "http://x:1"}, "cli", "")
	if err == nil {
		t.Error("expected error when memogo is down")
	}
	if !strings.Contains(err.Error(), "memogo") {
		t.Errorf("expected memogo error, got: %v", err)
	}
	// But in-memory should not have changed
	if _, ok := s.LookupRoute("offline.test"); ok {
		t.Error("offline route should not be in memory")
	}
}

func TestStatsReflectRoutes(t *testing.T) {
	s, _ := newTestStore(t)
	_, _ = s.LoadConfig()
	st := s.Stats()
	if st.ActiveRoutes != 1 {
		t.Errorf("expected 1 route loaded, got %d", st.ActiveRoutes)
	}
	if !st.MemogoUp {
		t.Error("expected memogo up")
	}
}
