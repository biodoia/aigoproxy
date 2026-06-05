package ports

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/biodoia/aigoproxy/internal/store"
)

func TestClaimRelease(t *testing.T) {
	mem := newFakeMemogo()
	a := New(mem)
	ctx := context.Background()
	// Claim
	r, err := a.Claim(ctx, 3000, "openwebui", "default port")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" || r.Port != 3000 {
		t.Errorf("got %+v, want ok/3000", r)
	}
	// Idempotent
	r2, err := a.Claim(ctx, 3000, "openwebui", "default port")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != "owned" {
		t.Errorf("idempotent claim should be 'owned', got %q", r2.Status)
	}
	// Conflicting claim
	r3, err := a.Claim(ctx, 3000, "other", "wanted it")
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != "taken" || r3.Owner != "openwebui" {
		t.Errorf("got %+v, want taken/openwebui", r3)
	}
	// Release by wrong owner
	if err := a.Release(ctx, 3000, "other"); err == nil {
		t.Error("expected error releasing with wrong owner")
	}
	// Release by right owner
	if err := a.Release(ctx, 3000, "openwebui"); err != nil {
		t.Fatal(err)
	}
	// Now free
	r4, _ := a.Claim(ctx, 3000, "anyone", "")
	if r4.Status != "ok" {
		t.Errorf("after release, got %+v, want ok", r4)
	}
}

func TestList(t *testing.T) {
	mem := newFakeMemogo()
	a := New(mem)
	ctx := context.Background()
	_, _ = a.Claim(ctx, 8080, "grafana", "")
	_, _ = a.Claim(ctx, 8082, "searxng", "")
	_, _ = a.Claim(ctx, 8000, "django", "")
	res, err := a.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Reservations) != 3 {
		t.Errorf("got %d reservations, want 3", len(res.Reservations))
	}
	// 8000, 8080, 8082 are in defaultInterestingPorts and now taken
	for _, p := range res.Free {
		if p == 8000 || p == 8080 || p == 8082 {
			t.Errorf("port %d should be marked taken", p)
		}
	}
	// 3000 should still be in Free
	found3000 := false
	for _, p := range res.Free {
		if p == 3000 {
			found3000 = true
		}
	}
	if !found3000 {
		t.Error("3000 should be in Free list")
	}
}

func TestNextFree(t *testing.T) {
	mem := newFakeMemogo()
	a := New(mem)
	ctx := context.Background()
	_, _ = a.Claim(ctx, 3000, "a", "")
	_, _ = a.Claim(ctx, 11000, "b", "")
	_, _ = a.Claim(ctx, 11001, "c", "")
	p, err := a.NextFree(ctx, []int{3000, 11000, 11001, 11002, 11003})
	if err != nil {
		t.Fatal(err)
	}
	if p != 11002 {
		t.Errorf("NextFree = %d, want 11002", p)
	}
	// all taken
	_, _ = a.Claim(ctx, 11002, "d", "")
	_, _ = a.Claim(ctx, 11003, "e", "")
	_, err = a.NextFree(ctx, []int{3000, 11000, 11001, 11002, 11003})
	if err == nil {
		t.Error("expected error when all candidates taken")
	}
}

func TestIsFree(t *testing.T) {
	mem := newFakeMemogo()
	a := New(mem)
	ctx := context.Background()
	free, _ := a.IsFree(ctx, 5555)
	if !free {
		t.Error("5555 should be free initially")
	}
	_, _ = a.Claim(ctx, 5555, "x", "")
	free, _ = a.IsFree(ctx, 5555)
	if free {
		t.Error("5555 should be taken after claim")
	}
}

// ─── fake memogo for tests (duplicated from store package to avoid import cycle) ──

type fakeMemogo struct {
	data map[string]map[string]fakePortEntry
}

type fakePortEntry struct {
	Data string
	Meta map[string]string
}

func newFakeMemogo() *fakeMemogo {
	return &fakeMemogo{data: map[string]map[string]fakePortEntry{}}
}

func (f *fakeMemogo) V2Store(ctx context.Context, ns, key string, content []byte, meta map[string]string) error {
	if f.data[ns] == nil {
		f.data[ns] = map[string]fakePortEntry{}
	}
	f.data[ns][key] = fakePortEntry{Data: string(content), Meta: meta}
	return nil
}

func (f *fakeMemogo) V2Get(ctx context.Context, ns, key string) ([]byte, map[string]string, error) {
	if e, ok := f.data[ns][key]; ok {
		return []byte(e.Data), e.Meta, nil
	}
	return nil, nil, nil
}

func (f *fakeMemogo) V2List(ctx context.Context, ns string) ([]MemogoEntry, error) {
	out := []MemogoEntry{}
	for k, e := range f.data[ns] {
		out = append(out, MemogoEntry{Namespace: ns, Key: k, Data: e.Data, Meta: e.Meta})
	}
	return out, nil
}

func (f *fakeMemogo) V2Delete(ctx context.Context, ns, key string) error {
	if f.data[ns] != nil {
		delete(f.data[ns], key)
	}
	return nil
}

// silence unused
var _ = store.MemogoEntry{}
var _ = json.Marshal
var _ = fmt.Sprintf
var _ = time.Now
