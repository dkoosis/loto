#!/usr/bin/env sh
#
# bd-run.sh — hand one git-hook event to beads, best-effort.
#
# Shared body behind every `hooks.d/<hook>/50-beads` entry. Not itself a chain
# entry: it lives in lib/, which the runner does not scan.
#
# bd installs its own hooks by repointing `core.hooksPath` at `.beads/hooks`,
# which would evict `.githooks` and every guard in it. So bd is invoked here
# through `bd hooks run <hook>`, its public entry point, and `.beads/hooks` is
# never routed to. This mirrors bd 1.1.2's own shim: the BEADS_HOOK_TIMEOUT
# budget, exit 124/142 read as a timeout, exit 3 read as "no database yet".
#
# Fail-open: no `bd` on PATH means the hook is a no-op.
#
# usage: bd-run.sh <hook-name> [<git hook args>...]

if [ $# -lt 1 ]; then
	echo "✗ bd-run.sh: usage: bd-run.sh <hook-name> [args...]" >&2
	exit 2
fi

hook=$1
shift

command -v bd >/dev/null 2>&1 || exit 0

export BD_GIT_HOOK=1
_bd_timeout=${BEADS_HOOK_TIMEOUT:-300}
_bd_used_perl=0

if command -v timeout >/dev/null 2>&1; then
	timeout "$_bd_timeout" bd hooks run "$hook" "$@"
	_bd_exit=$?
elif command -v gtimeout >/dev/null 2>&1; then
	gtimeout "$_bd_timeout" bd hooks run "$hook" "$@"
	_bd_exit=$?
elif command -v perl >/dev/null 2>&1; then
	_bd_used_perl=1
	perl -e 'alarm shift; exec @ARGV' "$_bd_timeout" bd hooks run "$hook" "$@"
	_bd_exit=$?
else
	echo "$hook: beads hook running without timeout; install coreutils or perl to enable BEADS_HOOK_TIMEOUT" >&2
	bd hooks run "$hook" "$@"
	_bd_exit=$?
fi

if [ "$_bd_exit" -eq 124 ] || { [ "$_bd_used_perl" -eq 1 ] && [ "$_bd_exit" -eq 142 ]; }; then
	echo "$hook: beads hook timed out after ${_bd_timeout}s — continuing without beads" >&2
	_bd_exit=0
fi

if [ "$_bd_exit" -eq 3 ]; then
	echo "$hook: beads database not initialized — skipping" >&2
	_bd_exit=0
fi

exit "$_bd_exit"
