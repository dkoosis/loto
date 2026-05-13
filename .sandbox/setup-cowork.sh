#!/usr/bin/env bash
# .sandbox/setup-cowork.sh — one-time dev environment setup for Cowork (linux-arm64).
#
# Cowork does not include Go or the prebuilt tools bundled for Codex.
# This script fetches Go and builds all prebuilt tools from source,
# caching everything to ~/.loto-cowork so subsequent sessions are instant.
#
# Usage (run once per fresh Cowork workspace):
#   bash .sandbox/setup-cowork.sh
#   source .sandbox/activate.sh
#
# After the script, `source .sandbox/activate.sh` puts everything on PATH.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SANDBOX_DIR="$REPO_DIR/.sandbox"

# --- Version pins (kept in sync with .sandbox/lib/Makefile.cross.mk) ---
GO_VERSION="1.25.1"
GOLANGCI_LINT_VER="v2.11.4"
GO_ARCH_LINT_VER="v1.14.0"
GOVULNCHECK_VER="v1.1.4"
GOFUMPT_VER="v0.9.2"
GOIMPORTS_VER="v0.39.0"
BAT_VER="v0.25.0"

# --- Paths ---
COWORK_CACHE="${LOTO_COWORK_HOME:-$HOME/.loto-cowork}"
GO_INSTALL_DIR="$COWORK_CACHE/go"
PREBUILT_DIR="$SANDBOX_DIR/bin/linux-arm64"

# Verify we're on linux-arm64 (this script is Cowork-specific).
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
if [ "$OS" != "linux" ] || [ "$ARCH" != "aarch64" ]; then
  echo "setup-cowork.sh is for linux/aarch64 only (got $OS/$ARCH)."
  echo "For other platforms, use 'make cross-arm64' on a dev machine."
  exit 1
fi

echo "=== loto cowork setup ==="
echo "  repo:   $REPO_DIR"
echo "  cache:  $COWORK_CACHE"
echo "  go:     $GO_VERSION"
echo ""

mkdir -p "$COWORK_CACHE" "$PREBUILT_DIR"

step() { echo "--- $1 ---"; }

# ------------------------------------------------------------------ step 1: Go
step "1. Go $GO_VERSION"
if "$GO_INSTALL_DIR/bin/go" version 2>/dev/null | grep -q "go$GO_VERSION"; then
  echo "  already installed"
else
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  TARBALL="go${GO_VERSION}.linux-arm64.tar.gz"
  URL="https://go.dev/dl/$TARBALL"
  echo "  downloading $URL ..."
  curl -fsSL --progress-bar "$URL" -o "$TMP/$TARBALL"
  echo "  extracting to $GO_INSTALL_DIR ..."
  rm -rf "$GO_INSTALL_DIR"
  mkdir -p "$GO_INSTALL_DIR"
  tar -xzf "$TMP/$TARBALL" -C "$GO_INSTALL_DIR" --strip-components=1
  echo "  go $($GO_INSTALL_DIR/bin/go version | awk '{print $3}')"
fi

export GOROOT="$GO_INSTALL_DIR"
export PATH="$GO_INSTALL_DIR/bin:$PATH"

# Use repo-local caches (same as lib-activate.sh)
export GOCACHE="$SANDBOX_DIR/cache/go-build"
export GOMODCACHE="$SANDBOX_DIR/cache/mod"
mkdir -p "$GOCACHE" "$GOMODCACHE"

# ------------------------------------------------------------------ step 2: Go tools
step "2. Go tools (go install)"

install_tool() {
  local name="$1" pkg="$2"
  if [ -f "$PREBUILT_DIR/$name" ]; then
    echo "  $name: already installed"
    return
  fi
  echo "  installing $name ..."
  CGO_ENABLED=0 go install -trimpath -ldflags='-s -w' "$pkg"
  local bin
  bin=$(go env GOPATH)/bin/$name
  if [ -f "$bin" ]; then
    cp "$bin" "$PREBUILT_DIR/$name"
    chmod +x "$PREBUILT_DIR/$name"
    echo "  $name: ok ($(du -h "$PREBUILT_DIR/$name" | cut -f1))"
  else
    echo "  WARNING: $name binary not found at $bin"
  fi
}

install_tool golangci-lint "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VER"
install_tool go-arch-lint  "github.com/fe3dback/go-arch-lint@$GO_ARCH_LINT_VER"
install_tool govulncheck   "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VER"
install_tool gofumpt       "mvdan.cc/gofumpt@$GOFUMPT_VER"
install_tool goimports     "golang.org/x/tools/cmd/goimports@$GOIMPORTS_VER"
install_tool snipe         "github.com/dkoosis/snipe@latest"
install_tool fo            "github.com/dkoosis/fo/cmd/fo@latest"

# ------------------------------------------------------------------ step 3: bat (binary release)
step "3. bat $BAT_VER"
if [ -f "$PREBUILT_DIR/bat" ]; then
  echo "  already installed"
else
  TMP2=$(mktemp -d)
  TARBALL="bat-${BAT_VER}-aarch64-unknown-linux-gnu.tar.gz"
  URL="https://github.com/sharkdp/bat/releases/download/$BAT_VER/$TARBALL"
  echo "  downloading $URL ..."
  curl -fsSL --progress-bar "$URL" | tar -xzf - -C "$TMP2"
  cp "$TMP2"/bat-*/bat "$PREBUILT_DIR/bat"
  chmod +x "$PREBUILT_DIR/bat"
  rm -rf "$TMP2"
  echo "  bat: ok ($(du -h "$PREBUILT_DIR/bat" | cut -f1))"
fi

# ------------------------------------------------------------------ step 4: dtree (shell script)
step "4. dtree"
if [ -f "$PREBUILT_DIR/dtree" ]; then
  echo "  already installed"
elif [ -f "$SANDBOX_DIR/codex/dtree" ]; then
  cp "$SANDBOX_DIR/codex/dtree" "$PREBUILT_DIR/dtree"
  chmod +x "$PREBUILT_DIR/dtree"
  echo "  dtree: installed from .sandbox/codex/dtree"
else
  echo "  WARNING: dtree source not found at .sandbox/codex/dtree"
fi

# ------------------------------------------------------------------ step 5: loto binary
step "5. loto"
if [ -f "$PREBUILT_DIR/loto" ]; then
  echo "  already built"
else
  echo "  building loto for linux/arm64 ..."
  cd "$REPO_DIR"
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o "$PREBUILT_DIR/loto" ./cmd/loto/
  echo "  loto: ok ($(du -h "$PREBUILT_DIR/loto" | cut -f1))"
fi

# ------------------------------------------------------------------ step 6: health check
step "6. Health check"
# Put prebuilt tools on PATH so doctor's `command -v` can see them.
export PATH="$PREBUILT_DIR:$PATH"
# Cowork has no /usr/local/bin install; treat prebuilt dir as the install dir
# so restore_sandbox_binaries is a safe no-op during diagnostics.
export INSTALL_DIR="$PREBUILT_DIR"

# shellcheck source=/dev/null
source "$SANDBOX_DIR/lib/lib-doctor.sh"

check_go_version
echo "  required tools:"
check_required_tools
echo "  optional tools:"
check_optional_tools

echo ""
echo "Tools in $PREBUILT_DIR:"
ls -lh "$PREBUILT_DIR" | awk '{print "  "$NF, $5}' | tail -n +2
echo ""
echo "Next: source .sandbox/activate.sh"
echo ""
doctor_exit "setup-cowork"
