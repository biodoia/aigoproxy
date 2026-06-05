#!/usr/bin/env bash
# install.sh — build and install aigoproxy as a systemd system service.
#
# Usage:  sudo ./deploy/install.sh
# Undo:   sudo ./deploy/uninstall.sh
#
# Requires: Go 1.21+, tailscale CLI, systemd, root (or sudo).
# Creates: /usr/local/bin/aigoproxy
#          /etc/aigoproxy/config.yaml
#          /var/lib/aigoproxy/{certs,logs,state}
#          aigoproxy user (system)
#          aigoproxy.service (systemd)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_NAME="aigoproxy"
INSTALL_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
RUN_DIR="/etc/aigoproxy"
DATA_DIR="/var/lib/aigoproxy"

[[ $EUID -eq 0 ]] || { echo "Must run as root (use sudo)"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found in PATH"; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found"; exit 1; }

info()  { printf "\033[1;34m[install]\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33m[install]\033[0m %s\n" "$*" >&2; }
fail()  { printf "\033[1;31m[install]\033[0m %s\n" "$*" >&2; exit 1; }

info "building $BIN_NAME from $PROJECT_DIR (static, stripped)"
cd "$PROJECT_DIR"
mkdir -p bin
# CGO_ENABLED=0 + -ldflags="-s -w" → fully static binary. Required for
# `setcap cap_net_bind_service` to work, because dynamic-linked Go
# binaries delegate capability handling to the dynamic loader.
CGO_ENABLED=0 GOSUMDB=off go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "bin/$BIN_NAME" \
  ./cmd/aigoproxy/

info "installing binary to $INSTALL_DIR/$BIN_NAME"
install -m 0755 "bin/$BIN_NAME" "$INSTALL_DIR/$BIN_NAME"

# Grant the binary the capability to bind privileged ports (<1024).
# This is the standard pattern for non-root web servers (similar to
# how nginx and Caddy do it).
if command -v setcap >/dev/null 2>&1; then
  if setcap 'cap_net_bind_service=+ep' "$INSTALL_DIR/$BIN_NAME" 2>/dev/null; then
    info "granted cap_net_bind_service (can bind :80, :443)"
  else
    warn "could not set cap_net_bind_service — service will need to bind to :8080+"
  fi
fi

info "creating aigoproxy system user"
if ! id aigoproxy >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin aigoproxy
fi

info "creating directories"
mkdir -p "$RUN_DIR" "$DATA_DIR/certs" "$DATA_DIR/logs"
chown -R aigoproxy:aigoproxy "$DATA_DIR"
chmod 0755 "$RUN_DIR"

if [ ! -f "$RUN_DIR/config.yaml" ]; then
  cat > "$RUN_DIR/config.yaml" <<'YAML'
# aigoproxy initial config
# Edit and `systemctl reload aigoproxy` to apply.
http_addr: ":80"
https_addr: ""                # set to ":443" once you've enabled HTTPS
base_domain: biodoia.ts.net    # your Tailscale tailnet
acme:
  enabled: false                # set true to enable Let's Encrypt
  email: ""
tailscale:
  use_funnel: true              # auto-register Tailscale Funnel listeners
routes: []
YAML
  info "wrote starter config to $RUN_DIR/config.yaml"
  chmod 0644 "$RUN_DIR/config.yaml"
fi

info "installing systemd system unit to $SYSTEMD_DIR"
sed "s|/usr/local/bin/$BIN_NAME|$INSTALL_DIR/$BIN_NAME|g" \
  "$SCRIPT_DIR/$BIN_NAME.service" > "$SYSTEMD_DIR/$BIN_NAME.service"
chmod 0644 "$SYSTEMD_DIR/$BIN_NAME.service"

systemctl daemon-reload
systemctl enable --now "$BIN_NAME.service"
sleep 1

if systemctl is-active --quiet "$BIN_NAME.service"; then
  info "✓ aigoproxy is running (PID $(systemctl show -p MainPID --value $BIN_NAME.service))"
else
  warn "service is not active. Check: journalctl -u $BIN_NAME -e"
fi

cat <<'DONE'

aigoproxy is installed and running as a systemd service.

Useful commands:
  systemctl status aigoproxy          # current state
  journalctl -u aigoproxy -f          # live logs
  systemctl reload aigoproxy          # apply config changes (no restart)
  systemctl restart aigoproxy         # full restart
  curl http://localhost/healthz       # smoke test
  curl -X POST http://localhost/mcp -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"aigoproxy_list"},"id":1}'

Config: $RUN_DIR/config.yaml
Data:   $DATA_DIR/{state,certs,logs}/
DONE
