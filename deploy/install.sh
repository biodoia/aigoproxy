#!/usr/bin/env bash
# install.sh — build and install aigoproxy as a systemd user service.
#
# Usage:  ./deploy/install.sh
# Undo:   ./deploy/uninstall.sh
#
# Requires: Go 1.21+, tailscale CLI, systemd (user mode).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_NAME="aigoproxy"
INSTALL_DIR="${AIGOPROXY_INSTALL_DIR:-$HOME/.local/bin}"
SYSTEMD_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

info()  { printf "\033[1;34m[install]\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33m[install]\033[0m %s\n" "$*" >&2; }
fail()  { printf "\033[1;31m[install]\033[0m %s\n" "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || fail "go not found in PATH"
command -v systemctl >/dev/null 2>&1 || fail "systemctl not found"
command -v tailscale >/dev/null 2>&1 || warn "tailscale CLI not found — Funnel won't auto-enable"

info "building $BIN_NAME from $PROJECT_DIR"
cd "$PROJECT_DIR"
mkdir -p bin
GOSUMDB=off go build -trimpath -ldflags="-s -w" -o "bin/$BIN_NAME" ./cmd/aigoproxy/

info "installing binary to $INSTALL_DIR/$BIN_NAME"
mkdir -p "$INSTALL_DIR"
install -m 0755 "bin/$BIN_NAME" "$INSTALL_DIR/$BIN_NAME"

# Make sure ~/.local/bin is on PATH for the user shell.
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  warn "$INSTALL_DIR is not on your PATH. Add this to ~/.bashrc:"
  warn "  export PATH=\$HOME/.local/bin:\$PATH"
fi

info "creating user data dir at ~/.aigoproxy"
mkdir -p "$HOME/.aigoproxy"
if [ ! -f "$HOME/.aigoproxy/config.yaml" ]; then
  cat > "$HOME/.aigoproxy/config.yaml" <<'YAML'
# aigoproxy initial config
# Edit and `systemctl --user reload aigoproxy` to apply.
http_addr: ":80"
https_addr: ":443"
base_domain: biodoia.ts.net
routes: []
YAML
  info "wrote starter config to ~/.aigoproxy/config.yaml"
fi

info "installing systemd user unit to $SYSTEMD_DIR"
mkdir -p "$SYSTEMD_DIR"
sed "s|/usr/local/bin/$BIN_NAME|$INSTALL_DIR/$BIN_NAME|g" \
  "$SCRIPT_DIR/$BIN_NAME.service" > "$SYSTEMD_DIR/$BIN_NAME.service"

systemctl --user daemon-reload
info "enabled and started aigoproxy"
systemctl --user enable --now "$BIN_NAME.service"

# Enable lingering so the service runs even when Sergio logs out.
if command -v loginctl >/dev/null 2>&1; then
  if ! loginctl show-user "$(whoami)" -p Linger 2>/dev/null | grep -q "yes"; then
    info "enabling lingering for $(whoami) (so aigoproxy survives logouts)"
    loginctl enable-linger "$(whoami)" 2>/dev/null || \
      warn "could not enable lingering — service will stop at logout"
  fi
fi

# Funnel reminder
if command -v tailscale >/dev/null 2>&1; then
  info "reminder: run \`tailscale funnel 80 on\` once (already in service ExecStartPost)"
  if ! tailscale funnel status 2>/dev/null | grep -q "80"; then
    warn "funnel for :80 doesn't appear active yet. Check: tailscale funnel status"
  fi
fi

cat <<'DONE'

aigoproxy is now installed and running.

Useful commands:
  systemctl --user status aigoproxy        # current state
  journalctl --user -u aigoproxy -f        # live logs
  systemctl --user reload aigoproxy        # apply config changes
  systemctl --user restart aigoproxy       # full restart
  curl http://localhost/healthz            # smoke test
  curl -X POST http://localhost/mcp -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"aigoproxy_list"},"id":1}'
DONE
