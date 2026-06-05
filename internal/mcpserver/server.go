// Package mcpserver exposes aigoproxy operations as MCP tools over HTTP.
// Implements a minimal subset of the Model Context Protocol (JSON-RPC 2.0)
// sufficient to drive aigoproxy from any MCP-aware agent.
//
// Endpoints:
//   POST /mcp        — JSON-RPC 2.0 endpoint
//   GET  /mcp/info   — server info
//
// Tools:
//   aigoproxy_list   — list all routes
//   aigoproxy_get    — get one route
//   aigoproxy_add    — add a route
//   aigoproxy_remove — remove a route
//   aigoproxy_log    — get recent access log
//   aigoproxy_stats  — get runtime stats
package mcpserver

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

// Server is the MCP server.
type Server struct {
	store  *store.Store
	logger *slog.Logger
}

// New returns a new MCP Server.
func New(s *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: s, logger: logger}
}

// Handler returns the http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleRPC)
	mux.HandleFunc("/mcp/info", s.handleInfo)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id"`
}

// JSON-RPC 2.0 response.
type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	Result  any    `json:"result,omitempty"`
	Error   *errObj `json:"error,omitempty"`
	ID      any    `json:"id"`
}

type errObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

var (
	mu      sync.Mutex
	lastID  int
)

func nextID() any {
	mu.Lock()
	defer mu.Unlock()
	lastID++
	return lastID
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.reply(w, rpcResponse{JSONRPC: "2.0", ID: nil, Error: &errObj{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" {
		s.reply(w, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &errObj{Code: -32600, Message: "jsonrpc must be 2.0"}})
		return
	}
	if req.ID == nil {
		req.ID = nextID()
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "tools/list":
		resp.Result = map[string]any{
			"tools": s.toolList(),
		}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &errObj{Code: -32602, Message: "invalid params"}
			break
		}
		result, err := s.toolCall(p.Name, p.Arguments)
		if err != nil {
			resp.Error = &errObj{Code: -32603, Message: err.Error()}
			break
		}
		resp.Result = result
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": "aigoproxy", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
	case "notifications/initialized":
		// no-op
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		resp.Error = &errObj{Code: -32601, Message: "method not found: " + req.Method}
	}
	s.reply(w, resp)
}

func (s *Server) reply(w http.ResponseWriter, r rpcResponse) {
	_ = json.NewEncoder(w).Encode(r)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    "aigoproxy",
		"version": "0.1.0",
		"tools":   s.toolList(),
	})
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) toolList() []toolDef {
	return []toolDef{
		{
			Name:        "aigoproxy_list",
			Description: "List all configured aigoproxy routes.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "aigoproxy_get",
			Description: "Get a single route by host.",
			InputSchema: map[string]any{
				"type":     "object",
				"properties": map[string]any{"host": map[string]any{"type": "string"}},
				"required": []string{"host"},
			},
		},
		{
			Name:        "aigoproxy_add",
			Description: "Add a new route.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":         map[string]any{"type": "string"},
					"upstream":     map[string]any{"type": "string"},
					"auth":         map[string]any{"type": "string", "enum": []string{"none", "tailscale", "funnel"}},
					"health":       map[string]any{"type": "string"},
					"strip_prefix": map[string]any{"type": "string"},
				},
				"required": []string{"host", "upstream"},
			},
		},
		{
			Name:        "aigoproxy_remove",
			Description: "Remove a route by host.",
			InputSchema: map[string]any{
				"type":     "object",
				"properties": map[string]any{"host": map[string]any{"type": "string"}},
				"required": []string{"host"},
			},
		},
		{
			Name:        "aigoproxy_log",
			Description: "Get recent access log entries (newest last).",
			InputSchema: map[string]any{
				"type":     "object",
				"properties": map[string]any{"limit": map[string]any{"type": "integer", "default": 50}},
			},
		},
		{
			Name:        "aigoproxy_stats",
			Description: "Get runtime stats (total requests, bytes proxied, etc.).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func (s *Server) toolCall(name string, args json.RawMessage) (any, error) {
	switch name {
	case "aigoproxy_list":
		return s.store.Config().Routes, nil
	case "aigoproxy_get":
		var p struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(args, &p)
		cfg := s.store.Config()
		for _, r := range cfg.Routes {
			if r.Host == p.Host {
				return r, nil
			}
		}
		return nil, fmt.Errorf("route %q not found", p.Host)
	case "aigoproxy_add":
		var p struct {
			Host        string `json:"host"`
			Upstream    string `json:"upstream"`
			Auth        string `json:"auth"`
			Health      string `json:"health"`
			StripPrefix string `json:"strip_prefix"`
			PathPrefix  string `json:"path_prefix"`
			}
			_ = json.Unmarshal(args, &p)
			_, err := s.store.AddRoute(config.Route{
			Host: p.Host, Upstream: p.Upstream, Auth: p.Auth, Health: p.Health, StripPrefix: p.StripPrefix, PathPrefix: p.PathPrefix,
			})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "host": p.Host}, nil
	case "aigoproxy_remove":
		var p struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(args, &p)
		if err := s.store.RemoveRoute(p.Host); err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "host": p.Host}, nil
	case "aigoproxy_log":
		var p struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Limit <= 0 {
			p.Limit = 50
		}
		return s.store.AccessLog(p.Limit), nil
	case "aigoproxy_stats":
		return s.store.Stats(), nil
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

// _ = time.Now to keep import if needed
var _ = time.Now
