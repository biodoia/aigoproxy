package mcpserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/biodoia/aigoproxy/internal/store"
)

func rpcCall(t *testing.T, h http.Handler, method string, params any) rpcResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp rpcResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rr.Body.String())
	}
	return resp
}

func setup(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	srv := New(s, slog.Default())
	return srv.Handler(), s
}

func TestMCPToolsList(t *testing.T) {
	h, _ := setup(t)
	resp := rpcCall(t, h, "tools/list", nil)
	if resp.Error != nil {
		t.Fatalf("error: %v", resp.Error.Message)
	}
	out, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(out), "aigoproxy_list") {
		t.Error("aigoproxy_list missing from tools list")
	}
	if !strings.Contains(string(out), "aigoproxy_add") {
		t.Error("aigoproxy_add missing from tools list")
	}
}

func TestMCPAddThenList(t *testing.T) {
	h, _ := setup(t)

	addResp := rpcCall(t, h, "tools/call", map[string]any{
		"name": "aigoproxy_add",
		"arguments": map[string]any{
			"host":     "test.example.com",
			"upstream": "http://127.0.0.1:8080",
			"auth":     "none",
		},
	})
	if addResp.Error != nil {
		t.Fatalf("add error: %s", addResp.Error.Message)
	}

	listResp := rpcCall(t, h, "tools/call", map[string]any{
		"name":      "aigoproxy_list",
		"arguments": map[string]any{},
	})
	if listResp.Error != nil {
		t.Fatalf("list error: %s", listResp.Error.Message)
	}
	routesJSON, _ := json.Marshal(listResp.Result)
	if !strings.Contains(string(routesJSON), "test.example.com") {
		t.Errorf("added route not in list: %s", routesJSON)
	}
}

func TestMCPRemove(t *testing.T) {
	h, s := setup(t)
	// add first
	_ = rpcCall(t, h, "tools/call", map[string]any{
		"name": "aigoproxy_add",
		"arguments": map[string]any{
			"host": "x.example.com", "upstream": "http://127.0.0.1:1",
		},
	})
	// then remove
	resp := rpcCall(t, h, "tools/call", map[string]any{
		"name":      "aigoproxy_remove",
		"arguments": map[string]any{"host": "x.example.com"},
	})
	if resp.Error != nil {
		t.Fatalf("remove error: %s", resp.Error.Message)
	}
	if len(s.Config().Routes) != 0 {
		t.Errorf("expected 0 routes after remove, got %d", len(s.Config().Routes))
	}
}

func TestMCPStats(t *testing.T) {
	h, _ := setup(t)
	resp := rpcCall(t, h, "tools/call", map[string]any{
		"name": "aigoproxy_stats",
	})
	if resp.Error != nil {
		t.Fatalf("error: %s", resp.Error.Message)
	}
	out, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(out), "total_requests") {
		t.Errorf("stats missing total_requests: %s", out)
	}
}

func TestMCPInitialize(t *testing.T) {
	h, _ := setup(t)
	resp := rpcCall(t, h, "initialize", nil)
	if resp.Error != nil {
		t.Fatalf("init error: %s", resp.Error.Message)
	}
	out, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(out), "aigoproxy") {
		t.Errorf("init response missing server name: %s", out)
	}
}

func TestMCPBadMethod(t *testing.T) {
	h, _ := setup(t)
	resp := rpcCall(t, h, "nonexistent", nil)
	if resp.Error == nil {
		t.Error("expected error for bad method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
}
