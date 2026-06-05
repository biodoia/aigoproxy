// Package acpserver exposes aigoproxy operations over a WebSocket channel
// as a JSON-RPC 2.0 server. ACP = "Agent Control Protocol" — a thin
// abstraction over the same operations as MCP, but with a persistent
// WebSocket connection suitable for long-running agent sessions.
//
// Endpoints:
//   GET /acp/ws     — WebSocket upgrade; bidirectional JSON-RPC 2.0
//   GET /acp/info   — server info
//
// On connect, the server pushes:
//   {type:"welcome", session_id, version, tools}
// Then receives JSON-RPC requests and returns responses, with optional
// "notify" messages from the server when state changes (route added,
// access log entry, etc.).
package acpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	nhooyrws "nhooyr.io/websocket"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)
// Server is the ACP server.
type Server struct {
	store  *store.Store
	logger *slog.Logger
}

// New returns a new ACP Server.
func New(s *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: s, logger: logger}
}

// Handler returns the http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/acp/ws", s.handleWS)
	mux.HandleFunc("/acp/info", s.handleInfo)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":    "aigoproxy-acp",
		"version": "0.1.0",
		"protocols": []string{"acp/1"},
		"endpoints": map[string]any{
			"ws":   "/acp/ws",
			"info": "/acp/info",
		},
	})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := nhooyrws.Accept(w, r, &nhooyrws.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logger.Error("acp: accept", "err", err)
		return
	}
	defer c.Close(1000, "bye")

	c.SetReadLimit(64 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID := fmt.Sprintf("acp-%d", time.Now().UnixNano())
	_ = c.Write(ctx, nhooyrws.MessageText, mustJSON(map[string]any{
		"type":       "welcome",
		"session_id": sessionID,
		"version":    "0.1.0",
		"tools": []string{
			"aigoproxy_list", "aigoproxy_get", "aigoproxy_add",
			"aigoproxy_remove", "aigoproxy_log", "aigoproxy_stats",
		},
		"timestamp": time.Now().Format(time.RFC3339Nano),
	}))

	s.logger.Info("acp: client connected", "id", sessionID)

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			s.logger.Info("acp: closed", "id", sessionID, "err", err)
			return
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			ID      any             `json:"id"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			_ = c.Write(ctx, nhooyrws.MessageText, mustJSON(map[string]any{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]any{"code": -32700, "message": "parse error"},
			}))
			continue
		}
		// dispatch
		resp := s.dispatch(ctx, &req)
		_ = c.Write(ctx, nhooyrws.MessageText, mustJSON(resp))
	}
}

func (s *Server) dispatch(ctx contextLike, req *struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      any             `json:"id"`
}) map[string]any {
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "ping":
		resp["result"] = map[string]any{"pong": time.Now().UnixMilli()}
	case "list_routes":
		resp["result"] = s.store.Config().Routes
	case "get_route":
		var p struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(req.Params, &p)
		for _, r := range s.store.Config().Routes {
			if r.Host == p.Host {
				resp["result"] = r
				return resp
			}
		}
		resp["error"] = map[string]any{"code": -32603, "message": "not found"}
	case "add_route":
		var p struct {
			Host, Upstream, Health, Auth, StripPrefix, PathPrefix string
		}
		_ = json.Unmarshal(req.Params, &p)
		_, err := s.store.AddRoute(config.Route{
			Host: p.Host, Upstream: p.Upstream, Health: p.Health, Auth: p.Auth, StripPrefix: p.StripPrefix, PathPrefix: p.PathPrefix,
		})
		if err != nil {
			resp["error"] = map[string]any{"code": -32603, "message": err.Error()}
			return resp
		}
		resp["result"] = map[string]any{"status": "ok"}
		// notify
		// (would need a broadcast channel — omitted for now)
	case "remove_route":
		var p struct {
			Host string `json:"host"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if err := s.store.RemoveRoute(p.Host); err != nil {
			resp["error"] = map[string]any{"code": -32603, "message": err.Error()}
			return resp
		}
		resp["result"] = map[string]any{"status": "ok"}
	case "log":
		var p struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Limit <= 0 {
			p.Limit = 50
		}
		resp["result"] = s.store.AccessLog(p.Limit)
	case "stats":
		resp["result"] = s.store.Stats()
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
	}
	return resp
}

func routeFromArgs(p struct{ Host, Upstream, Health, Auth, StripPrefix, PathPrefix string }) routeAddArgs {
	return routeAddArgs{
		Host: p.Host, Upstream: p.Upstream, Health: p.Health, Auth: p.Auth, StripPrefix: p.StripPrefix, PathPrefix: p.PathPrefix,
	}
}

// routeAddArgs is a thin alias to keep dispatch readable.
type routeAddArgs = struct {
	Host, Upstream, Health, Auth, StripPrefix, PathPrefix string
}

// _ ensures routeAddArgs stays available for future use even though the
// current dispatch path uses config.Route directly.
var _ = routeAddArgs{}

type contextLike = interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

var _ atomic.Bool // keep import for future broadcast use

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
