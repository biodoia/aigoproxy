// Package tui is a minimal terminal dashboard for aigoproxy. It is a
// pure-stdlib line reader (no bubbletea / tcell dep) to keep the binary
// small and the build fast. For richer UI, see the Web UI.
package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/biodoia/aigoproxy/internal/config"
	"github.com/biodoia/aigoproxy/internal/store"
)

// UI is the interactive REPL.
type UI struct {
	store  *store.Store
	logger *slog.Logger
	in     io.Reader
	out    io.Writer
}

// New returns a new UI.
func New(s *store.Store, logger *slog.Logger) *UI {
	return &UI{
		store:  s,
		logger: logger,
		in:     os.Stdin,
		out:    os.Stdout,
	}
}

// Run starts the REPL and blocks until ctx is cancelled or EOF.
func (u *UI) Run(ctx context.Context) error {
	fmt.Fprintln(u.out, "aigoproxy TUI — type 'help' for commands")
	scanner := bufio.NewScanner(u.in)
	for {
		fmt.Fprint(u.out, "> ")
		if !scanner.Scan() {
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := u.dispatch(ctx, line); err != nil {
			fmt.Fprintf(u.out, "error: %v\n", err)
		}
	}
}

func (u *UI) dispatch(ctx context.Context, line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "help", "h", "?":
		u.cmdHelp()
	case "list", "ls":
		u.cmdList()
	case "add":
		return u.cmdAdd(parts[1:])
	case "remove", "rm":
		return u.cmdRemove(parts[1:])
	case "show", "get":
		return u.cmdShow(parts[1:])
	case "log":
		u.cmdLog(parts[1:])
	case "stats":
		u.cmdStats()
	case "reload":
		fmt.Fprintln(u.out, "reload not yet wired from TUI — restart aigoproxy to pick up changes")
	case "exit", "quit", "q":
		fmt.Fprintln(u.out, "bye")
		return errExit
	default:
		fmt.Fprintf(u.out, "unknown command %q (type 'help')\n", parts[0])
	}
	return nil
}

var errExit = fmt.Errorf("exit")

func (u *UI) cmdHelp() {
	fmt.Fprintln(u.out, `commands:
  list, ls                list all routes
  show <host>             show details for one route
  add <host> <upstream>   add a new route
  remove, rm <host>       remove a route
  log [N]                 show last N access log entries (default 20)
  stats                   show runtime stats
  reload                  reload config (todo)
  exit, quit, q           leave the TUI
  help, h, ?              this message`)
}

func (u *UI) cmdList() {
	cfg := u.store.Config()
	if len(cfg.Routes) == 0 {
		fmt.Fprintln(u.out, "(no routes)")
		return
	}
	for _, r := range cfg.Routes {
		flag := " "
		if !r.Enabled {
			flag = "-"
		}
		fmt.Fprintf(u.out, "  %s %-40s → %s [%s]\n", flag, r.Host, r.Upstream, r.Auth)
	}
}

func (u *UI) cmdAdd(parts []string) error {
	if len(parts) < 2 {
		return fmt.Errorf("usage: add <host> <upstream> [auth] [health]")
	}
	host := parts[0]
	upstream := parts[1]
	auth := "none"
	if len(parts) >= 3 {
		auth = parts[2]
	}
	health := ""
	if len(parts) >= 4 {
		health = parts[3]
	}
	if _, err := u.store.AddRoute(config.Route{
		Host: host, Upstream: upstream, Auth: auth, Health: health,
	}); err != nil {
		return err
	}
	fmt.Fprintf(u.out, "added %s\n", host)
	return nil
}

func (u *UI) cmdRemove(parts []string) error {
	if len(parts) < 1 {
		return fmt.Errorf("usage: remove <host>")
	}
	if err := u.store.RemoveRoute(parts[0]); err != nil {
		return err
	}
	fmt.Fprintf(u.out, "removed %s\n", parts[0])
	return nil
}

func (u *UI) cmdShow(parts []string) error {
	if len(parts) < 1 {
		return fmt.Errorf("usage: show <host>")
	}
	cfg := u.store.Config()
	for _, r := range cfg.Routes {
		if r.Host == parts[0] {
			out, _ := json.MarshalIndent(r, "", "  ")
			fmt.Fprintln(u.out, string(out))
			return nil
		}
	}
	return fmt.Errorf("route %q not found", parts[0])
}

func (u *UI) cmdLog(parts []string) {
	limit := 20
	if len(parts) > 0 {
		_, _ = fmt.Sscanf(parts[0], "%d", &limit)
	}
	for _, e := range u.store.AccessLog(limit) {
		fmt.Fprintf(u.out, "  %s  %s  %-3d  %-40s  %s  %s\n",
			e.Time.Format("15:04:05"), e.Method, e.Status, e.Host, e.Path, durStr(e.LatencyMs))
	}
}

func (u *UI) cmdStats() {
	s := u.store.Stats()
	fmt.Fprintf(u.out, "  active routes:    %d\n", s.ActiveRoutes)
	fmt.Fprintf(u.out, "  total requests:   %d\n", s.TotalRequests)
	fmt.Fprintf(u.out, "  bytes proxied:    %d\n", s.BytesProxied)
	fmt.Fprintf(u.out, "  started:          %s\n", s.StartedAt.Format(time.RFC3339))
	if !s.LastRequestAt.IsZero() {
		fmt.Fprintf(u.out, "  last request:     %s\n", s.LastRequestAt.Format(time.RFC3339))
	}
}

func durStr(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// Avoid unused imports: keep http and url referenced (used in future remote
// mode).
var _ = http.Client{}
var _ = url.Parse
