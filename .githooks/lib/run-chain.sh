#!/usr/bin/env sh
#
# run-chain.sh — the one chain-runner behind every .githooks/<hook> dispatcher.
#
# ── THE CONTRACT ──────────────────────────────────────────────────────────
# Write a new guard by dropping ONE executable file into
# `.githooks/hooks.d/<hook>/`. Nothing else is edited: no dispatcher, no
# Makefile, no registry.
#
#   layout      .githooks/<hook>                 dispatcher (3 lines; do not edit)
#               .githooks/hooks.d/<hook>/NN-name entry (yours)
#               .githooks/lib/*.sh               shared bodies (not entries)
#
#   name        NN-kebab-name, NN two digits. Byte (LC_ALL=C) order is run
#               order, so the number IS the position. Reserved bands:
#                 00-09  environment / setup
#                 10-49  loto's own guards
#                 50-89  third-party integrations (bd is 50-beads)
#                 90-99  advisory / reporting, last
#
#   mode        Must be executable (`chmod +x`, committed as mode 100755).
#               A non-executable file is SKIPPED silently by git's own rules,
#               so `make hooks` prints a ⚠ row for one.
#
#   argv        Each entry gets the hook's argv verbatim ("$@").
#
#   stdin       Hooks git feeds on stdin (pre-push and kin) have it captured
#               once and REPLAYED to every entry from the start, so an entry
#               that reads stdin does not starve the next one. Every other
#               hook's entries get /dev/null, matching git.
#
#   env         LOTO_GIT_HOOK=<hook> is exported to every entry.
#
#   exit        0 = pass. Non-zero STOPS the chain: no later entry runs and
#               the dispatcher exits with that same status, which is what git
#               sees. Preserves the old flat hooks, where loto's refusal kept
#               bd from running.
#
#   fail-open   An entry that needs a tool decides for itself. The runner has
#               no opinion; the convention entries follow is
#               `command -v <tool> >/dev/null 2>&1 || exit 0`.
#
#   skipped     `*.orig`, `*.sample`, `*.disabled` and dotfiles are ignored,
#               so a hook can be parked without deleting it.
#
# ── WHY ───────────────────────────────────────────────────────────────────
# `core.hooksPath` holds exactly one directory, so every tool that installs
# hooks by repointing it (bd, husky, pre-commit, a global ~/.config/git/hooks)
# silently evicts the last one. The chain gives each a file instead of the
# whole slot. bd is a chain ENTRY (`hooks.d/<hook>/50-beads` calls `bd hooks
# run`, bd's public entry point) — `core.hooksPath` is never repointed at
# `.beads/hooks`, and `make hooks` refuses to overwrite a foreign value.
#
# usage: run-chain.sh <hook-name> [<git hook args>...]

set -u

if [ $# -lt 1 ]; then
	echo "✗ run-chain.sh: usage: run-chain.sh <hook-name> [args...]" >&2
	exit 2
fi

hook=$1
shift

githooks_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd) || exit 2

# LOTO_HOOK_PROBE=1 (loto-ea8y.11): answer before touching the chain at all —
# no stdin capture, no chain_dir read, no entry runs. `loto doctor` execs the
# EFFECTIVE hook (direct .githooks/<hook>, or a foreign forwarder that lands
# here) with this set and empty stdin, and reads reachable iff this exact
# line comes back. Every dispatcher only execs into this file, so answering
# once here covers pre-commit/post-checkout/reference-transaction/etc. alike.
#
# repo_top is resolved via `git rev-parse --show-toplevel`, not a plain `cd
# .. && pwd` off githooks_dir: git resolves PHYSICALLY (symlinks followed),
# while a logical shell `cd`/`pwd` does not — on macOS $TMPDIR sits behind
# /var -> /private/var, so the two diverge there and doctor's own toplevel
# (also `git rev-parse --show-toplevel`, runtime.go) would never match a
# logically-computed string. Using git on both sides makes them the same
# computation by construction, not just usually equal.
if [ "${LOTO_HOOK_PROBE:-}" = "1" ]; then
	repo_top=$(git -C "$githooks_dir" rev-parse --show-toplevel 2>/dev/null) || exit 2
	printf 'loto-hook-probe %s %s\n' "$hook" "$repo_top"
	exit 0
fi

chain_dir="$githooks_dir/hooks.d/$hook"

# No directory, or an empty one, is a legitimate "this hook does nothing".
[ -d "$chain_dir" ] || exit 0

# Byte order, explicitly. A locale-sensitive glob would order `10-a` against
# `10_a` differently on two machines; the chain must be identical everywhere.
entries=$(
	for e in "$chain_dir"/*; do
		[ -e "$e" ] || continue
		printf '%s\n' "${e##*/}"
	done | LC_ALL=C sort
)
[ -n "$entries" ] || exit 0

# Hooks git feeds on stdin. One entry reading it would leave the next with an
# empty pipe, so capture once and hand every entry its own read from the top.
stdin_file=""
case "$hook" in
pre-push | pre-receive | post-receive | update | proc-receive | push-to-checkout | reference-transaction)
	stdin_file=$(mktemp "${TMPDIR:-/tmp}/loto-hook-stdin.XXXXXX") || exit 2
	trap 'rm -f "$stdin_file"' EXIT HUP INT TERM
	cat >"$stdin_file"
	;;
esac

LOTO_GIT_HOOK=$hook
export LOTO_GIT_HOOK

old_ifs=$IFS
IFS='
'
for name in $entries; do
	IFS=$old_ifs
	entry="$chain_dir/$name"

	case "$name" in
	.* | *.orig | *.sample | *.disabled) continue ;;
	esac
	[ -f "$entry" ] || continue
	[ -x "$entry" ] || continue

	if [ -n "$stdin_file" ]; then
		"$entry" "$@" <"$stdin_file"
	else
		"$entry" "$@" </dev/null
	fi
	rc=$?

	if [ "$rc" -ne 0 ]; then
		echo "✗ $hook: hooks.d/$hook/$name exit=$rc — chain stopped" >&2
		exit "$rc"
	fi

	IFS='
'
done
IFS=$old_ifs

exit 0
