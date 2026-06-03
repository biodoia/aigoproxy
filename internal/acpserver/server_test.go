package acpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nhooyrws "nhooyr.io/websocket"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

func setupACP(t *testing.T) (*store.Store, http.Handler) {
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
	srv := New(s, logger)
	return s, srv.Handler()
}

func connectACP(t *testing.T, h http.Handler) *nhooyrws.Conn {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/acp/ws"
	c, _, err := nhooyrws.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close(1000, "test done") })
	return c
}

// readMsg reads a single text message or times out.
func readMsg(t *testing.T, c *nhooyrws.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	c.SetReadLimit(1 << 20)
	_, data, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v (data=%s)", err, string(data))
	}
	return m
}

func TestACPWelcome(t *testing.T) {
	_, h := setupACP(t)
	c := connectACP(t, h)
	msg := readMsg(t, c, 2*time.Second)
	if msg["type"] != "welcome" {
		t.Errorf("first message type = %v, want welcome", msg["type"])
	}
	if _, ok := msg["session_id"]; !ok {
		t.Error("welcome missing session_id")
	}
}

func TestACPListRoutes(t *testing.T) {
	_, h := setupACP(t)
	c := connectACP(t, h)
	_ = readMsg(t, c, 2*time.Second) // welcome

	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  "list_routes",
		"id":      1,
	}
	data, _ := json.Marshal(req)
	if err := c.Write(context.Background(), nhooyrws.MessageText, data); err != nil {
		t.Fatal(err)
	}
	resp := readMsg(t, c, 2*time.Second)
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", resp["jsonrpc"])
	}
	if resp["id"].(float64) != 1 {
		t.Errorf("id = %v, want 1", resp["id"])
	}
	if _, ok := resp["result"]; !ok {
		t.Error("response missing result")
	}
}

func TestACPPing(t *testing.T) {
	_, h := setupACP(t)
	c := connectACP(t, h)
	_ = readMsg(t, c, 2*time.Second)

	req := map[string]any{"jsonrpc": "2.0", "method": "ping", "id": 2}
	data, _ := json.Marshal(req)
	if err := c.Write(context.Background(), nhooyrws.MessageText, data); err != nil {
		t.Fatal(err)
	}
	resp := readMsg(t, c, 2*time.Second)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %v", resp)
	}
	if _, ok := res["pong"]; !ok {
		t.Error("ping response missing pong")
	}
}

func TestACPAddThenRemove(t *testing.T) {
	s, h := setupACP(t)
	c := connectACP(t, h)
	_ = readMsg(t, c, 2*time.Second)

	// add
	addReq := map[string]any{
		"jsonrpc": "2.0",
		"method":  "add_route",
		"params": map[string]any{
			"host":     "acp.test.ts.net",
			"upstream": "http://127.0.0.1:9000",
		},
		"id": 3,
	}
	data, _ := json.Marshal(addReq)
	_ = c.Write(context.Background(), nhooyrws.MessageText, data)
	resp := readMsg(t, c, 2*time.Second)
	if _, ok := resp["error"]; ok {
		t.Fatalf("add error: %v", resp["error"])
	}
	if len(s.Config().Routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(s.Config().Routes))
	}

	// remove
	rmReq := map[string]any{
		"jsonrpc": "2.0",
		"method":  "remove_route",
		"params":  map[string]any{"host": "acp.test.ts.net"},
		"id":      4,
	}
	data, _ = json.Marshal(rmReq)
	_ = c.Write(context.Background(), nhooyrws.MessageText, data)
	resp = readMsg(t, c, 2*time.Second)
	if _, ok := resp["error"]; ok {
		t.Fatalf("remove error: %v", resp["error"])
	}
	if len(s.Config().Routes) != 0 {
		t.Errorf("expected 0 routes, got %d", len(s.Config().Routes))
	}
}

func TestACPUnknownMethod(t *testing.T) {
	_, h := setupACP(t)
	c := connectACP(t, h)
	_ = readMsg(t, c, 2*time.Second)

	req := map[string]any{"jsonrpc": "2.0", "method": "nope", "id": 5}
	data, _ := json.Marshal(req)
	_ = c.Write(context.Background(), nhooyrws.MessageText, data)
	resp := readMsg(t, c, 2*time.Second)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got %v", resp)
	}
	if errObj["code"].(float64) != -32601 {
		t.Errorf("code = %v, want -32601", errObj["code"])
	}
}

// avoid unused import
var _ = config.Route{}
