#!/usr/bin/env bash
# check-pack-drift.sh — fail CI when a consuming repo's copied pack rules have
# drifted from the upstream cc-plugins pack.
#
# The pack ships as copied files under .golangci-rules/: bugclasses.go (a
# verbatim copy of gorules/rules.go) and modern.go (a verbatim copy of
# gorules/modern.go). Copies rot silently: upstream fixes a rule, the copy
# stays stale, or someone hand-edits the copy. This makes both loud.
#
# For each local file it compares two things against its upstream twin:
#   1. version token — the //pack:version line.
#   2. content hash  — sha256 (catches local edits that DIDN'T bump the version).
#
# Vendor this into the consuming repo (.golangci-rules/) and run it in CI after
# golangci-lint. Network-soft: an unreachable upstream WARNS and exits 0 so a
# transient outage never breaks the build; a real mismatch exits 1.
#
# Usage: check-pack-drift.sh [local-file ...]   (needs gh auth; PACK_UPSTREAM_REF pins a tag)
#   default: every pack file present under .golangci-rules/
#   Upstream twin is chosen by basename: bugclasses.go → rules.go, else same name.
#   PACK_UPSTREAM_BASE: a public raw-content base URL, fetched with curl instead.
set -euo pipefail

# cc-plugins is a PRIVATE repo: raw.githubusercontent.com answers 404 without a
# token, which the first version of this script read as "unreachable" and
# skipped — a silent no-op in every consumer. Fetch through `gh api` (uses the
# caller's auth: keychain locally, GH_TOKEN in CI) and fall back to curl only
# for a PACK_UPSTREAM_BASE that points somewhere public.
PACK_REPO="${PACK_UPSTREAM_REPO:-dkoosis/cc-plugins}"
PACK_REF="${PACK_UPSTREAM_REF:-main}"
PACK_PATH="plugins/lintbrush/gorules"
BASE="${PACK_UPSTREAM_BASE:-}"

die()  { printf '✗ pack-drift: %s\n' "$*" >&2; exit 1; }
warn() { printf '⚠ pack-drift: %s\n' "$*" >&2; }
sha()  { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; }
vtok() { grep -m1 '//pack:version' "$1" 2>/dev/null | awk '{print $2}'; }

upstream_name() {
	case "$(basename "$1")" in
		bugclasses.go) echo rules.go ;;
		*)             basename "$1" ;;
	esac
}

# fetch <upstream-name> <out> — 0 on success, 1 when the upstream is unreadable.
fetch() {
	if [[ -n "$BASE" ]]; then
		curl -fsSL --max-time 10 "$BASE/$1" -o "$2" 2>/dev/null; return
	fi
	command -v gh >/dev/null 2>&1 || return 1
	gh api -H 'Accept: application/vnd.github.raw' \
		"repos/$PACK_REPO/contents/$PACK_PATH/$1?ref=$PACK_REF" >"$2" 2>/dev/null
}

files=("$@")
if [[ ${#files[@]} -eq 0 ]]; then
	for f in .golangci-rules/bugclasses.go .golangci-rules/modern.go .golangci-rules/optin_rawstatewrite.go; do
		[[ -f "$f" ]] && files+=("$f")
	done
	[[ ${#files[@]} -gt 0 ]] || die "no pack files under .golangci-rules/"
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
rc=0
for local in "${files[@]}"; do
	[[ -f "$local" ]] || { warn "local copy not found: $local"; rc=1; continue; }
	name="$(upstream_name "$local")"
	if ! fetch "$name" "$tmp"; then
		warn "cannot read upstream $PACK_REPO@$PACK_REF:$PACK_PATH/$name (no gh, no auth, or offline) — skipping drift check for $local"
		continue
	fi
	lv="$(vtok "$local")"; uv="$(vtok "$tmp")"
	if [[ "$(sha "$local")" == "$(sha "$tmp")" ]]; then
		printf '✓ pack-drift: %s in sync (%s)\n' "$local" "${lv:-unversioned}"
		continue
	fi
	if [[ "$lv" != "$uv" ]]; then
		printf '✗ pack-drift: %s version drift: local %s vs upstream %s. Re-run adopt-pack.sh.\n' "$local" "${lv:-none}" "${uv:-none}" >&2
	else
		printf '✗ pack-drift: %s content drift at the same version (%s). A local hand-edit? Re-run adopt-pack.sh or bump upstream.\n' "$local" "$lv" >&2
	fi
	rc=1
done
exit $rc
