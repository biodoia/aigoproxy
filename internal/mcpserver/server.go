// Package mcpserver exposes aigoproxy operations as MCP tools over HTTP.
// Implements a minimal subset of the Model Context Protocol (JSON-RPC 2.0)
// sufficient to drive aigoproxy from any MCP-aware agent.
//
// Endpoints:
//   POST /mcp        — JSON-RPC 2.0 endpoint
//   GET  /mcp/info   — server info
//
// Tools:
//   aigoproxy_list    — list all routes
//   aigoproxy_get     — get one route
//   aigoproxy_add     — add a route
//   aigoproxy_remove  — remove a route
//   aigoproxy_log     — get recent access log
//   aigoproxy_stats   — get runtime stats
//   aigoproxy_inspect — probe an upstream and report auth presence
//   aigoproxy_register — one-shot: inspect + add + screenshot + funnel
//   aigoproxy_scan    — port-scan localhost for HTTP services
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/detector"
	"github.com/biodoia/aigoproxy/internal/ports"
	"github.com/biodoia/aigoproxy/internal/store"
)

// Server is the MCP server.
type Server struct {
	store    *store.Store
	ports    *ports.Allocator
	logger   *slog.Logger
	onChange func() // optional: invoked after add/remove so caller can reload
	scanFn   func() []any
}

// SetOnRouteChanged registers a callback fired after aigoproxy_add and
// aigoproxy_register so the caller can reload the proxy and refresh
// screenshots in the background.
func (s *Server) SetOnRouteChanged(fn func()) { s.onChange = fn }

// SetScan registers a port-scan function the MCP server can call for
// aigoproxy_scan. Returns the slice of suggestions.
func (s *Server) SetScan(fn func() []any) { s.scanFn = fn }

// New returns a new MCP Server. The port allocator is optional (may
// be nil for tests); when non-nil, the aigoproxy_ports_* tools
// become available.
func New(s *store.Store, portAlloc *ports.Allocator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: s, ports: portAlloc, logger: logger}
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
		{
			Name:        "aigoproxy_inspect",
			Description: "Probe an upstream URL and return whether it has its own auth (HTML form, redirect to /login, etc). Use before aigoproxy_register to know if you need to wrap it with auth.",
			InputSchema: map[string]any{
				"type":     "object",
				"properties": map[string]any{"upstream": map[string]any{"type": "string"}},
				"required": []string{"upstream"},
			},
		},
		{
			Name:        "aigoproxy_register",
			Description: "One-shot register: inspect the upstream, pick a sensible auth, add the route, capture a screenshot, configure Tailscale Funnel (if path_prefix is set). Returns the new route and the inspection result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host":        map[string]any{"type": "string"},
					"upstream":    map[string]any{"type": "string"},
					"path_prefix": map[string]any{"type": "string"},
					"auth":        map[string]any{"type": "string", "enum": []string{"none", "tailscale", "funnel", "auto"}},
				},
				"required": []string{"host", "upstream"},
			},
		},
		{
			Name:        "aigoproxy_scan",
			Description: "Scan localhost for HTTP services and return a list of {port, title, has_auth, suggested_host, suggested_path} so the agent can pick which to expose. Mirrors /api/rescan.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "aigoproxy_ports_list",
			Description: "List port reservations and the well-known free ports from the aigoproxy port-allocator. Mirrors GET /api/ports/list.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "aigoproxy_ports_claim",
			Description: "Reserve a port for an owner (e.g. 'openwebui'). Returns {status: 'ok'|'owned'|'taken', port, owner?}. Use this BEFORE starting any new service that needs a port. If the port is taken, the caller should pick the next port in its fallback list and try again.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"port":  map[string]any{"type": "integer", "description": "Port number to reserve (1024-65535)"},
					"owner": map[string]any{"type": "string", "description": "Service name claiming the port (e.g. 'openwebui')"},
					"note":  map[string]any{"type": "string", "description": "Optional human-readable note"},
				},
				"required": []string{"port", "owner"},
			},
		},
		{
			Name:        "aigoproxy_ports_release",
			Description: "Release a previously-claimed port. The owner field must match the original claim or the call fails.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"port":  map[string]any{"type": "integer", "description": "Port to release"},
					"owner": map[string]any{"type": "string", "description": "Owner name used in the original claim"},
				},
				"required": []string{"port", "owner"},
			},
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
	case "aigoproxy_inspect":
		var p struct {
			Upstream string `json:"upstream"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Upstream == "" {
			return nil, fmt.Errorf("upstream required")
		}
		res, err := detector.Inspect(context.Background(), p.Upstream)
		if err != nil {
			return nil, err
		}
		return res, nil
	case "aigoproxy_register":
		var p struct {
			Host       string `json:"host"`
			Upstream   string `json:"upstream"`
			PathPrefix string `json:"path_prefix"`
			Auth       string `json:"auth"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Host == "" || p.Upstream == "" {
			return nil, fmt.Errorf("host and upstream required")
		}
		// 1. Inspect
		insp, _ := detector.Inspect(context.Background(), p.Upstream)
		// 2. Pick auth. The semantics:
		//   - "tailscale": request must come from a device in the tailnet
		//     (Tailscale-User or X-Forwarded-For-Tailscale header). Use
		//     this for routes accessed only via tailnet.
		//   - "funnel": request comes from the public internet via
		//     Tailscale Funnel. No client auth header is set; the
		//     network path is the auth. Use this for routes that
		//     are exposed publicly (typically anything with path_prefix).
		//   - "none": no auth. The user is fully responsible.
		auth := p.Auth
		if auth == "" || auth == "auto" {
			switch {
			case p.PathPrefix != "":
				// Publicly exposed via Tailscale Funnel
				auth = "funnel"
			case insp != nil && insp.HasAuth:
				// Service has its own login
				auth = "none"
			default:
				// Service has no login and we are NOT publicly
				// exposing it → require tailnet membership.
				auth = "tailscale"
			}
		}
		// 3. Add route
		_, err := s.store.AddRoute(config.Route{
			Host: p.Host, Upstream: p.Upstream, Auth: auth, PathPrefix: p.PathPrefix,
		})
		if err != nil {
			return nil, err
		}
		// 4. Reload proxy + trigger screenshot (background)
		if s.onChange != nil {
			go s.onChange()
		}
		return map[string]any{
			"status":         "ok",
			"host":           p.Host,
			"auth_applied":   auth,
			"inspection":     insp,
			"funnel_path":    p.PathPrefix,
		}, nil
	case "aigoproxy_scan":
		// Returns the most recent port scan results. If none, run one now.
		if s.scanFn != nil {
			return s.scanFn(), nil
		}
		return nil, nil
	case "aigoproxy_ports_list":
		if s.ports == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}
		return s.ports.List(context.Background())
	case "aigoproxy_ports_claim":
		if s.ports == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}
		var p struct {
			Port  int    `json:"port"`
			Owner string `json:"owner"`
			Note  string `json:"note"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("bad args: %w", err)
		}
		if p.Port == 0 || p.Owner == "" {
			return nil, fmt.Errorf("port and owner are required")
		}
		return s.ports.Claim(context.Background(), p.Port, p.Owner, p.Note)
	case "aigoproxy_ports_release":
		if s.ports == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}
		var p struct {
			Port  int    `json:"port"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("bad args: %w", err)
		}
		if p.Port == 0 || p.Owner == "" {
			return nil, fmt.Errorf("port and owner are required")
		}
		if err := s.ports.Release(context.Background(), p.Port, p.Owner); err != nil {
			return nil, err
		}
		return map[string]any{"status": "released", "port": p.Port}, nil
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
