# go-sandbox v0.2.0 synced 2026-04-05
#!/usr/bin/env bash
# go-sandbox lib-setup.sh — setup actions
# Makes the environment correct. Downloads, installs, builds.
# Sourced, not executed. Requires: REPO_DIR, PREBUILT_DIR, INSTALL_DIR, SANDBOX_DIR.
# Requires: lib-doctor.sh sourced first (for have, warn, fatal).
# Requires: project.conf sourced (for HAS_LOCAL_REPLACES).

# Download Go modules, optionally stripping local replace directives.
# Uses `go mod edit -dropreplace` instead of sed to avoid deleting legitimate
# require entries that happen to share a module path with a local replace.
download_go_modules() {
  local label="${1:-downloaded}"
  cd "$REPO_DIR" || { fatal "cannot cd to REPO_DIR: $REPO_DIR"; return 1; }
  if [ "$HAS_LOCAL_REPLACES" = "true" ] && grep -q 'replace.*=> \.\.' go.mod; then
    (
      cp go.mod go.mod.bak
      cp go.sum go.sum.bak 2>/dev/null || true
      trap 'mv -f go.mod.bak go.mod; [ -f go.sum.bak ] && mv -f go.sum.bak go.sum' EXIT
      # Drop each local replace via go mod edit (safe — only touches replace directives)
      grep 'replace.*=> \.\.' go.mod | awk '{print $2}' | while read -r mod; do
        go mod edit -dropreplace="$mod"
      done
      go mod download
    )
    local rc=$?
    [ "$rc" -eq 0 ] && echo "  go modules $label (stripped local-only modules)"
    return "$rc"
  fi
  go mod download && echo "  go modules $label"
}

# Copy all sandbox binaries (tools + project binaries) from .sandbox/bin/ to INSTALL_DIR
install_sandbox_binaries() {
  if [ -d "$PREBUILT_DIR" ]; then
    echo "  copying from $PREBUILT_DIR ..."
    for tool in "$PREBUILT_DIR"/*; do
      [ -f "$tool" ] || continue
      local toolname size
      toolname=$(basename "$tool")
      size=$(du -h "$tool" | cut -f1)
      cp "$tool" "$INSTALL_DIR/$toolname"
      chmod +x "$INSTALL_DIR/$toolname"
      echo "  installed $toolname ($size)"
    done
  else
    echo "  WARNING: prebuilt dir not found: $PREBUILT_DIR"
  fi
}

# Build or skip snipe index
build_snipe_index() {
  if [ -f "$REPO_DIR/.snipe/index.db" ]; then
    echo "  index.db exists from git — skipping rebuild (maintenance.sh will refresh)"
  elif have snipe; then
    cd "$REPO_DIR" || { fatal "cannot cd to REPO_DIR: $REPO_DIR"; return 1; }
    echo "  no index.db found, building ..."
    snipe index --embed-mode=off --enrich=false 2>&1 | tail -5 && echo "  snipe index built" || echo "  snipe index skipped"
  else
    echo "  snipe not found, skipping"
  fi
}

# Rebuild snipe index (for maintenance — always rebuilds if snipe available)
refresh_snipe_index() {
  if have snipe; then
    cd "$REPO_DIR" || { fatal "cannot cd to REPO_DIR: $REPO_DIR"; return 1; }
    snipe index --embed-mode=off --enrich=false 2>/dev/null && echo "  snipe index rebuilt" || echo "  snipe index skipped"
  fi
}

# Compile test binaries without running them (warms Go build cache)
warm_test_cache() {
  cd "$REPO_DIR" || { fatal "cannot cd to REPO_DIR: $REPO_DIR"; return 1; }
  echo "  compiling test binaries (runs no tests) ..."
  if go test -run='^$' -count=1 ./... >/dev/null 2>&1; then
    echo "  test cache warm"
  else
    warn "test cache warmup failed (non-fatal)"
  fi
}

# Install fo stub if real fo is not available
install_fo_stub() {
  if have fo; then
    echo "  fo already installed ($(fo --version 2>/dev/null | head -1 || echo unknown))"
  else
    echo "  fo not in prebuilts, installing stub (passthrough)"
    cat > "$INSTALL_DIR/fo" <<'STUB'
#!/bin/sh
# fo stub: pass stdin through when real fo is unavailable
case "$1" in --version|version) echo "fo-stub 0.0.0"; exit 0 ;; esac
cat
STUB
    chmod +x "$INSTALL_DIR/fo"
    warn "fo stub installed (passthrough — not the real tool)"
  fi
}

# Install jscpd via npm (optional)
install_jscpd() {
  if have npm; then
    echo "  npm install -g jscpd@4 ..."
    npm install -g jscpd@4 --silent 2>&1 | tail -3 && echo "  installed jscpd" || warn "optional tool jscpd failed to install"
  else
    echo "  npm not found, skipping"
  fi
}

# Alias fdfind -> fd (Ubuntu)
setup_fd_alias() {
  if have fdfind && ! have fd; then
    ln -sf "$(command -v fdfind)" "$INSTALL_DIR/fd"
    echo "  aliased fdfind -> fd"
  else
    echo "  nothing to do"
  fi
}
