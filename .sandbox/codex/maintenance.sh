#!/usr/bin/env bash
# Codex cached container refresh for loto.
# Keep lightweight — setup.sh already installed tools.
set -euo pipefail

SANDBOX_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_DIR="$(cd "$SANDBOX_DIR/.." && pwd)"
cd "$REPO_DIR"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) ARCH="amd64" ;;
esac
PREBUILT_DIR="$SANDBOX_DIR/bin/linux-$ARCH"
INSTALL_DIR="/usr/local/bin"

# shellcheck source=/dev/null
source "$SANDBOX_DIR/lib/lib-doctor.sh"
# shellcheck source=/dev/null
source "$SANDBOX_DIR/lib/lib-setup.sh"

echo "=== loto maintenance ==="

if ! download_go_modules "refreshed"; then
  fatal "go mod download failed"
fi

restore_sandbox_binaries
check_go_version

echo "  Required tools:"
check_required_tools

doctor_exit "maintenance"
