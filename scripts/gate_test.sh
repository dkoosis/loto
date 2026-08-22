#!/usr/bin/env bash
#
# gate_test.sh — drive scripts/gate.sh with deliberately broken producers.
#
# Guards the loto-fcbp contract: a producer that dies before emitting findings
# must surface its raw diagnostic, and a producer that emits real findings must
# still render through fo rather than dumping raw output.
#
# Run: make scriptcheck   (or: bash scripts/gate_test.sh)

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate=$here/gate.sh
fails=0
ran=0

# expect <name> <want-status> <want-substring> -- <gate args...>
expect() {
	local name=$1 want_status=$2 want=$3
	shift 3
	[ "${1:-}" = "--" ] && shift
	ran=$((ran + 1))

	local got
	got=$("$gate" "$@" 2>&1)
	local status=$?

	if [ "$status" -ne "$want_status" ]; then
		printf '✗ %s: exit=%d want=%d\n' "$name" "$status" "$want_status"
		fails=$((fails + 1))
		return
	fi
	case "$got" in
	*"$want"*) ;;
	*)
		printf '✗ %s: output missing %s\n' "$name" "$want"
		printf '%s\n' "$got" | sed 's/^/    /'
		fails=$((fails + 1))
		return
		;;
	esac
	printf '✓ %s\n' "$name"
}

# A toolchain failure is the exact shape that shipped as "+ no findings".
expect diag-infra-failure-shows-cause 1 'toolchain not available' -- \
	vet diag -- bash -c 'echo "go: download go1.99.0: toolchain not available" >&2; exit 1'

expect diag-infra-failure-names-tool 1 '✗ vet did not run' -- \
	vet diag -- bash -c 'echo boom >&2; exit 1'

expect diag-empty-producer-is-explicit 3 'wrote nothing' -- \
	vet diag -- bash -c 'exit 3'

# A real finding still renders through fo; the raw stream must not take over.
expect diag-real-findings-render 1 'ERROR unreachable code' -- \
	vet diag -- bash -c 'echo "internal/x/y.go:4:2: unreachable code"; exit 1'

expect diag-clean-producer-passes 0 'no findings' -- \
	vet diag -- bash -c 'exit 0'

# sarif mode holds stderr back until the run fails empty — the old recipes
# sent it to /dev/null, so a crashed linter left no trace at all.
expect sarif-stderr-survives-failure 1 'cannot load packages' -- \
	lint sarif -- bash -c 'echo "level=error cannot load packages" >&2; exit 1'

# A compile error under -json yields no test JSON at all.
expect testjson-build-failure-shows-cause 2 'undefined: Frobnicate' -- \
	test testjson -- bash -c 'echo "internal/x/y.go:9:2: undefined: Frobnicate" >&2; exit 2'

if [ "$fails" -ne 0 ]; then
	printf '\n✗ gate.sh: %d of %d checks failed\n' "$fails" "$ran"
	exit 1
fi
printf '\n✓ gate.sh: %d checks passed\n' "$ran"
