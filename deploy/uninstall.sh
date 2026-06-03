#!/usr/bin/env bash
# uninstall.sh — stop the aigoproxy systemd user service and remove files.
#
# Usage: ./deploy/uninstall.sh [--purge]
#
# Without --purge: stops service, removes binary, keeps config + certs.
# With --purge: also removes ~/.aigoproxy.

set -euo pipefail

PURGE=0
for arg in "$@"; do
  case "$arg" in
    --purge) PURGE=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 1 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_NAME="aigoproxy"
INSTALL_DIR="${AIGOPROXY_INSTALL_DIR:-$HOME/.local/bin}"
SYSTEMD_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"

info()  { printf "\033[1;34m[uninstall]\033[0m %s\n" "$*"; }
fail()  { printf "\033[1;31m[uninstall]\033[0m %s\n" "$*" >&2; exit 1; }

command -v systemctl >/dev/null 2>&1 || fail "systemctl not found"

if systemctl --user is-active --quiet "$BIN_NAME.service" 2>/dev/null; then
  info "stopping $BIN_NAME.service"
  systemctl --user stop "$BIN_NAME.service"
fi

if systemctl --user is-enabled --quiet "$BIN_NAME.service" 2>/dev/null; then
  info "disabling $BIN_NAME.service"
  systemctl --user disable "$BIN_NAME.service"
fi

UNIT="$SYSTEMD_DIR/$BIN_NAME.service"
if [ -f "$UNIT" ]; then
  rm -f "$UNIT"
  systemctl --user daemon-reload
  info "removed $UNIT"
fi

BIN="$INSTALL_DIR/$BIN_NAME"
if [ -f "$BIN" ]; then
  rm -f "$BIN"
  info "removed $BIN"
fi

if [ "$PURGE" = "1" ]; then
  if [ -d "$HOME/.aigoproxy" ]; then
    rm -rf "$HOME/.aigoproxy"
    info "purged ~/.aigoproxy (config, certs, logs)"
  fi
  # Tailscale Funnel stays on — leave that to the user.
  info "note: \`tailscale funnel 80 off\` is not run automatically"
else
  info "kept config and certs in ~/.aigoproxy (use --purge to remove)"
fi

echo "aigoproxy uninstalled."
