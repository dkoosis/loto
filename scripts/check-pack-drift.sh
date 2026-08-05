#!/usr/bin/env bash
# check-pack-drift.sh — fail CI when a consuming repo's copied bugclasses rules
# have drifted from the upstream cc-plugins pack.
#
# The pack ships as a copied file (.golangci-rules/bugclasses.go, a verbatim
# copy of gorules/rules.go). Copies rot silently: upstream fixes a rule, the
# copy stays stale, or someone hand-edits the copy. This makes both loud.
#
# It compares two things:
#   1. version token — the //pack:version line in the local copy vs upstream.
#   2. content hash  — sha256 of the local copy vs upstream rules.go (catches
#      local edits that DIDN'T bump the version).
#
# Vendor this into the consuming repo (e.g. .golangci-rules/) and run it in CI
# after golangci-lint. Network-soft: an unreachable upstream WARNS and exits 0
# so a transient outage never breaks the build; a real mismatch exits 1.
#
# Usage: check-pack-drift.sh [path-to-local-bugclasses.go]
#   default local path: .golangci-rules/bugclasses.go
set -euo pipefail

UPSTREAM_URL="${PACK_UPSTREAM_URL:-https://raw.githubusercontent.com/dkoosis/cc-plugins/main/plugins/lintbrush/gorules/rules.go}"
LOCAL="${1:-.golangci-rules/bugclasses.go}"

die()  { printf '✗ pack-drift: %s\n' "$*" >&2; exit 1; }
warn() { printf '⚠ pack-drift: %s\n' "$*" >&2; }

[[ -f "$LOCAL" ]] || die "local copy not found: $LOCAL"

sha()  { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; }
vtok() { grep -m1 '//pack:version' "$1" 2>/dev/null | awk '{print $2}'; }

upstream="$(mktemp)"
trap 'rm -f "$upstream"' EXIT
if ! curl -fsSL --max-time 10 "$UPSTREAM_URL" -o "$upstream" 2>/dev/null; then
	warn "upstream unreachable ($UPSTREAM_URL) — skipping drift check"
	exit 0
fi

lv="$(vtok "$LOCAL")"; uv="$(vtok "$upstream")"
lh="$(sha "$LOCAL")";  uh="$(sha "$upstream")"

if [[ "$lh" == "$uh" ]]; then
	printf '✓ pack-drift: in sync (%s)\n' "${lv:-unversioned}"
	exit 0
fi

if [[ "$lv" != "$uv" ]]; then
	die "version drift: local ${lv:-none} vs upstream ${uv:-none}. Re-copy rules.go → $LOCAL and re-tune exclusions."
fi
die "content drift: $LOCAL differs from upstream at the same version ($lv). A local hand-edit? Re-copy or bump upstream."
