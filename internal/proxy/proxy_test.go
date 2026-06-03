package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

// startTestUpstream starts a small HTTP test server and returns its URL.
// It registers a cleanup hook to close itself when the test ends.
func startTestUpstream(t *testing.T, handler http.HandlerFunc) (url string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newTestProxy(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(s, logger), s
}

func TestLookupDispatch(t *testing.T) {
	// upstream that just echoes the Host header it received
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from upstream, host=" + r.Host))
	})

	px, s := newTestProxy(t)
	if _, err := s.AddRoute(config.Route{
		Host: "app.test.ts.net", Upstream: upstream,
	}); err != nil {
		t.Fatal(err)
	}
	if err := px.Reload(); err != nil {
		t.Fatal(err)
	}

	// request with matching host
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello from upstream") {
		t.Errorf("body = %q, expected to contain 'hello from upstream'", body)
	}
}

func TestUnknownHostShowsRoutes(t *testing.T) {
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{Host: "known.test.ts.net", Upstream: "http://127.0.0.1:1"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "unknown.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "known.test.ts.net") {
		t.Error("expected dashboard to list known hosts")
	}
}

func TestDisabledRouteSkipped(t *testing.T) {
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {})

	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{Host: "app.test.ts.net", Upstream: upstream})
	if err := s.SetRouteEnabled("app.test.ts.net", false); err != nil {
		t.Fatal(err)
	}
	if err := px.Reload(); err != nil {
		t.Fatal(err)
	}

	// disabled route → dashboard (404)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404 for disabled route", rec.Code)
	}
}

func TestAuthNoneAllows(t *testing.T) {
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{Host: "app.test.ts.net", Upstream: upstream, Auth: "none"})
	px.Reload()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestAuthTailscaleWithoutHeaders(t *testing.T) {
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{Host: "app.test.ts.net", Upstream: upstream, Auth: "tailscale"})
	px.Reload()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.test.ts.net"
	// no Tailscale headers → 403
	px.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("status = %d, want 403 (no tailscale headers)", rec.Code)
	}
}

func TestAuthTailscaleWithHeaders(t *testing.T) {
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{Host: "app.test.ts.net", Upstream: upstream, Auth: "tailscale"})
	px.Reload()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "app.test.ts.net"
	req.Header.Set("Tailscale-User", "sergio@example.com")
	px.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 (tailscale headers present)", rec.Code)
	}
}

func TestStripPrefix(t *testing.T) {
	var seenPath string
	upstream := startTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	})
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{
		Host: "app.test.ts.net", Upstream: upstream, Auth: "none",
		StripPrefix: "/api",
	})
	px.Reload()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/users", nil)
	req.Host = "app.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seenPath != "/v1/users" {
		t.Errorf("upstream path = %q, want /v1/users", seenPath)
	}
}

func TestUpstreamDown(t *testing.T) {
	// an upstream that refuses connections
	px, s := newTestProxy(t)
	_, _ = s.AddRoute(config.Route{
		Host: "dead.test.ts.net", Upstream: "http://127.0.0.1:1", Auth: "none", // closed port
	})
	px.Reload()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "dead.test.ts.net"
	px.ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// silence unused import warning
var _ = time.Now
var _ = os.Getenv
