<div align="center">

# 🌐 aigoproxy

**Tailscale-aware reverse proxy. One funnel, many subdomains, automatic HTTPS.**

Reverse proxy that routes `*.biodoia.ts.net` (or any Tailscale Funnel
domain) to local services. Single binary, exposes Web UI + TUI + MCP
server + ACP server. Stdlib-only Go where possible.

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Stack: Stdlib](https://img.shields.io/badge/stack-stdlib%20first-blueviolet.svg)]()
[![Single Binary](https://img.shields.io/badge/binary-single-success.svg)]()
[![MCP: yes](https://img.shields.io/badge/MCP-yes-orange.svg)]()
[![ACP: yes](https://img.shields.io/badge/ACP-yes-orange.svg)]()

[Features](#-features) · [Quick Start](#-quick-start) · [Architecture](#-architecture) · [MCP/ACP](#-mcp--acp) · [Docs](docs/) · [Contributing](CONTRIBUTING.md)

</div>

---

## 📖 The elevator pitch

Tailscale Funnel gives you a public HTTPS endpoint for a single local port.
If you want to expose **multiple** services, you need a reverse proxy in
front of it. That's aigoproxy: it sits on :80, dispatches by Host header to
upstream services, optionally terminates TLS via Let's Encrypt, and
exposes a Web UI + TUI + MCP + ACP for management.

It's the network layer of [Autoschei](https://github.com/biodoia/autoschei)
— every public URL in the fleet goes through aigoproxy.

> *One funnel. Many sites. Automatic HTTPS. Single binary.*

## 🖼️ The visual

```
         Tailscale Funnel  (yourdomain.ts.net:443, public)
                    │
                    │ HTTPS
                    ▼
       ┌────────────────────────┐
       │      aigoproxy         │
       │  ====================  │
       │  :80  :443             │
       │  SNI / Host dispatch   │
       │  Let's Encrypt ACME    │
       │  Health checks         │
       │  Access log            │
       └────┬──────┬──────┬─────┘
            │      │      │
       ┌────▼──┐ ┌─▼──┐ ┌─▼────┐
       │ App 1 │ │App2│ │ App3 │
       │ :8080 │ │:80 │ │ :3000│
       │ local │ │LAN │ │ local│
       └───────┘ └───┘ └──────┘
```

## ✨ Features

- 🚇 **Tailscale Funnel ready** — bind :80, run `tailscale funnel 80 on`,
  get public HTTPS for every registered subdomain
- 🔀 **Host-based routing** — virtual hosts map to upstream URLs
- 🔐 **Three auth modes** — `none`, `tailscale` (must be on tailnet), `funnel`
- 🔁 **Hot reload** — add/remove routes via API without restart
- 🩺 **Health probes** — configurable path per route, 30s interval
- 📊 **Web UI** — dashboard, route management, live access log
- 🖥️ **TUI** — stdlib-only REPL, no ncurses
- 🤖 **MCP server** — drive aigoproxy from any MCP-aware agent
- ⚡ **ACP server** — WebSocket JSON-RPC for long-lived agent sessions
- 🪪 **Memory safe** — Go + net/http stdlib, single binary
- 📦 **Zero external runtime deps** — no systemd deps, no Lua, no Node

## 🚀 Quick start

```bash
# 1. install
go install github.com/biodoia/aigoproxy/cmd/aigoproxy@latest

# 2. create initial config
mkdir -p ~/.aigoproxy
cat > ~/.aigoproxy/config.yaml <<EOF
http_addr: ":80"
https_addr: ":443"
base_domain: biodoia.ts.net
routes:
  - host: app1.biodoia.ts.net
    upstream: http://127.0.0.1:8080
    auth: none
EOF

# 3. run
aigoproxy
```

Visit <http://localhost/dashboard> (oh wait, the dashboard is on the
_dashboard_ host — add `dashboard.biodoia.ts.net` to your routes and
point it at the dashboard port). For local testing, hit
<http://localhost:8080> after running with `-addr :8080`.

To expose publicly:

```bash
tailscale funnel 80 on
# now app1.biodoia.ts.net works from anywhere
```

## 📦 Deployment

```bash
# build + install as systemd user service (recommended for h24)
./deploy/install.sh

# status / logs
systemctl --user status aigoproxy
journalctl --user -u aigoproxy -f

# apply config changes (SIGHUP, no restart)
systemctl --user reload aigoproxy

# uninstall
./deploy/uninstall.sh            # keeps config
./deploy/uninstall.sh --purge    # nukes everything
```

The systemd unit:

- binds :80 (via `AmbientCapabilities=CAP_NET_BIND_SERVICE` — no root)
- runs `tailscale funnel 80 on` after start (idempotent)
- enables systemd-lingering so the daemon survives logout
- hardens the process (ProtectSystem, PrivateTmp, no new privs, etc.)
- sends SIGTERM for graceful stop, SIGHUP for config reload

## 🎯 Why?

Tailscale Funnel is great but it exposes a single port. If you run three
services on the same machine, you need a way to route `subdomain1` to
:8080, `subdomain2` to :9000, etc. Caddy and Nginx can do this but
they're big. aigoproxy is one Go binary that:

- Discovers routes from a YAML file
- Exposes the route list via a Web UI
- Exposes the route list via a TUI
- Exposes the route list via MCP (so agents can edit it)
- Exposes the route list via ACP (WebSocket for live state)
- Issues Let's Encrypt certs via DNS-01 (Wave 2)
- Probes each upstream for health
- Logs every access
- Talks to no other process

## 🏗️ Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                        aigoproxy                              │
│  ─────────────────────────────────────────────────────────  │
│                                                              │
│   internal/config      YAML state at ~/.aigoproxy/config    │
│   internal/store       in-memory + persistence + access log  │
│   internal/proxy       reverse proxy core (httputil)         │
│   internal/webui       dashboard at /, /routes, /api/...     │
│   internal/tui         REPL at -tui flag                     │
│   internal/mcpserver   JSON-RPC 2.0 at /mcp                  │
│   internal/acpserver   WebSocket JSON-RPC at /acp/ws         │
│   internal/acme        Let's Encrypt manager (Wave 2)         │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

Each layer is independent. Replace the proxy, swap the storage, plug in
a different MCP server — they're all behind interfaces.

## 🔌 MCP and ACP

aigoproxy speaks MCP out of the box at `POST /mcp`. Drive it from any
agent:

```bash
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"aigoproxy_add","arguments":{"host":"foo.biodoia.ts.net","upstream":"http://127.0.0.1:9000","auth":"tailscale"}},"id":1}'
```

Tools: `aigoproxy_list`, `aigoproxy_get`, `aigoproxy_add`, `aigoproxy_remove`,
`aigoproxy_log`, `aigoproxy_stats`.

ACP is the WebSocket variant at `ws://localhost:8080/acp/ws` for
long-lived agent sessions. Same operations over a persistent connection.

📖 [Full MCP/ACP spec →](docs/MCP_ACP.md)

## 📚 Documentation

| File | What it covers |
|------|----------------|
| [docs/MCP_ACP.md](docs/MCP_ACP.md) | MCP and ACP wire protocol |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | systemd unit, Tailscale Funnel setup, TLS |
| [docs/CONFIG.md](docs/CONFIG.md) | Full YAML config reference |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Dev setup, code style |
| [SECURITY.md](SECURITY.md) | Vulnerability disclosure |

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## 🛡️ Security

See [SECURITY.md](SECURITY.md).

## 📜 License

[MIT](LICENSE) — © 2026 Biodoia / Sergio Martinelli

## 🌐 Related projects

- [framegotui](https://github.com/biodoia/framegotui) — the AI-native desktop framework
- [saidisigo](https://github.com/biodoia/saidisigo) — voice agent runtime
- [fomonad](https://github.com/biodoia/fomonad) — orchestrator
- [memogo](https://github.com/biodoia/memogo) — long-term memory
- [biblaigo](https://github.com/biodoia/biblaigo) — search layer

## ✍️ Author

**Sergio Martinelli** — [@Biodoia](https://t.me/Biodoia) — Manjaro Sway, Italy 🇮🇹

<div align="center">

*If aigoproxy saved you from juggling five Caddyfiles, ⭐ the repo.*

</div>
