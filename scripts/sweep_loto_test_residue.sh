#!/usr/bin/env bash
#
# sweep_loto_test_residue.sh — find (and, opt-in, quarantine) test-run
# residue in a loto state directory's agents/ and session/ subdirs.
#
# loto-bt6c: tests that forgot to redirect $HOME (three fixed by this bead,
# guarded structurally against a repeat by a per-package TestMain floor)
# minted thousands of throwaway agent + session records into dk's real
# ~/.loto instead of a temp HOME. This script finds them by SHAPE, not by
# having watched a run happen: a real agent or session record's filename is
# always the UUID v4 hex layout identity.newUUID emits — see
# internal/identity/registry.go's agentIDShape — because CLAUDE_CODE_SESSION_ID
# for a genuine Claude Code session is itself a UUID. Anything else under
# agents/ or session/ (pin-<nanos>-<n>, alice-*, somebody-else-<pid>,
# short-hex fragments, ...) cannot be a legitimate record and is residue by
# construction, regardless of when it was written.
#
# DEFAULT IS DRY-RUN — this script only ever reports, unless --apply is
# given. Even with --apply it does not delete: matched files are MOVED into
# a timestamped quarantine directory alongside the state dir, never
# unlinked, so the sweep is reversible right up until dk empties the
# quarantine dir himself.
#
# THIS SCRIPT HAS NOT BEEN RUN against dk's real ~/.loto. Running it —
# dry-run included — is his call: see loto-bt6c.
#
# usage:
#   scripts/sweep_loto_test_residue.sh [--home DIR] [--apply] [--list]
#
#   --home DIR   loto state root to scan (default: $HOME/.loto)
#   --apply      move residue into a quarantine dir instead of just reporting
#   --list       print every residue path, not just counts + a sample
#
# exit: 0 = ran (dry-run or apply); 2 = usage error; nonzero mid-run I/O
# error from `mv` bubbles up via set -e in the apply loop.

set -uo pipefail

# uuidShape mirrors internal/identity/registry.go's agentIDShape: the exact
# hex layout newUUID emits. It is deliberately not a strict RFC 4122 v4
# check (no version/variant bit enforcement) for the same reason the Go
# regex isn't — a filename either has this shape or it wasn't minted by
# loto's own uuid generator.
uuidShape='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'

home_dir="${HOME:-}/.loto"
apply=0
list=0

while [ $# -gt 0 ]; do
	case "$1" in
	--home)
		[ $# -ge 2 ] || {
			printf '✗ --home requires a directory argument\n' >&2
			exit 2
		}
		home_dir=$2
		shift 2
		;;
	--apply)
		apply=1
		shift
		;;
	--list)
		list=1
		shift
		;;
	-h | --help)
		sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		printf '✗ unknown argument: %s\n' "$1" >&2
		exit 2
		;;
	esac
done

if [ ! -d "$home_dir" ]; then
	printf 'ℹ no loto state dir at %s — nothing to sweep\n' "$home_dir"
	exit 0
fi

# classify_dir scans one subdir (agents|session) for *.json files, splitting
# basenames into real (uuid-shaped) vs residue (everything else). Residue
# paths are appended to $residue_file; real-count/residue-count are echoed
# as two space-separated integers so the caller can accumulate totals
# without relying on subshell-local variables.
classify_dir() {
	local dir=$1 residue_file=$2
	local real=0 residue=0
	local f base
	if [ ! -d "$dir" ]; then
		printf '0 0\n'
		return
	fi
	for f in "$dir"/*.json; do
		[ -e "$f" ] || continue # empty dir: the glob didn't match anything
		base=$(basename "$f" .json)
		if printf '%s' "$base" | grep -qE "$uuidShape"; then
			real=$((real + 1))
		else
			residue=$((residue + 1))
			printf '%s\n' "$f" >>"$residue_file"
		fi
	done
	printf '%d %d\n' "$real" "$residue"
}

residue_list=$(mktemp)
trap 'rm -f "$residue_list"' EXIT

agents_dir="$home_dir/agents"
session_dir="$home_dir/session"

read -r agents_real agents_residue <<<"$(classify_dir "$agents_dir" "$residue_list")"
read -r session_real session_residue <<<"$(classify_dir "$session_dir" "$residue_list")"

total_real=$((agents_real + session_real))
total_residue=$((agents_residue + session_residue))

mode_word="dry-run — nothing moved"
[ "$apply" -eq 1 ] && mode_word="apply"

printf 'ℹ residue sweep (%s): real=%d residue=%d — agents(real=%d residue=%d) session(real=%d residue=%d)\n' \
	"$mode_word" "$total_real" "$total_residue" \
	"$agents_real" "$agents_residue" "$session_real" "$session_residue"

if [ "$total_residue" -eq 0 ]; then
	printf '✓ nothing matches the residue shape under %s\n' "$home_dir"
	exit 0
fi

# Group residue by its longest non-numeric leading prefix so the summary
# reads as "3383 pin-*, 1 somebody-else-*, ..." instead of one line per
# file — design.md bans repeated field names per row, and 4000 raw paths
# would blow past any reasonable stdout budget.
printf 'ℹ residue prefixes:\n'
sed 's#.*/##; s/\.json$//' "$residue_list" |
	sed -E 's/[0-9][0-9-]*$//' |
	sort | uniq -c | sort -k1,1rn -k2,2 |
	awk '{printf "  %6d  %s*\n", $1, $2}'

if [ "$list" -eq 1 ]; then
	printf 'ℹ residue paths:\n'
	sort "$residue_list" | sed 's/^/  /'
fi

if [ "$apply" -eq 0 ]; then
	printf 'ℹ dry-run only — rerun with --apply to quarantine these %d files\n' "$total_residue"
	exit 0
fi

quarantine="$home_dir/.sweep-quarantine-$(date +%Y%m%dT%H%M%S)"
mkdir -p "$quarantine/agents" "$quarantine/session"

set -e
moved=0
while IFS= read -r f; do
	case "$f" in
	*/agents/*) dest="$quarantine/agents/" ;;
	*/session/*) dest="$quarantine/session/" ;;
	*) dest="$quarantine/" ;;
	esac
	mv "$f" "$dest"
	moved=$((moved + 1))
done <"$residue_list"
set +e

printf '✓ moved %d residue files into %s (not deleted — remove that dir yourself once you have checked it)\n' "$moved" "$quarantine"
