#!/usr/bin/env bash
#
# makefile_check_test.sh — drive scripts/makefile_check.sh with fixture
# Makefiles that should and should not trip it.
#
# Guards loto-uekt's AC #4: the regression check itself must catch a recipe
# that goes back to relying on `-e`/`pipefail` from .SHELLFLAGS, and must not
# cry wolf on the shapes this repo legitimately uses — `||` fallbacks, a `|`
# inside a jq or awk program, and a variable assignment's continuation lines.
#
# Run: make scriptcheck   (or: bash scripts/makefile_check_test.sh)

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$here/.." && pwd)
checker=$here/makefile_check.sh
fails=0
ran=0

tmp=$(mktemp -d) || {
	echo "makefile_check_test.sh: mktemp failed" >&2
	exit 2
}
trap 'rm -rf "$tmp"' EXIT

ok() {
	ran=$((ran + 1))
	echo "  ✓ $1"
}

bad() {
	ran=$((ran + 1))
	fails=$((fails + 1))
	echo "  ✗ $1"
	[ -n "${2:-}" ] && echo "      $2"
}

# want_pass <name> <makefile-body>
want_pass() {
	local name=$1 body=$2
	printf '%s\n' "$body" >"$tmp/Makefile"
	if out=$(bash "$checker" "$tmp/Makefile" 2>&1); then
		ok "$name"
	else
		bad "$name" "expected clean, got: $out"
	fi
}

# want_fail <name> <makefile-body>
want_fail() {
	local name=$1 body=$2
	printf '%s\n' "$body" >"$tmp/Makefile"
	if out=$(bash "$checker" "$tmp/Makefile" 2>&1); then
		bad "$name" "expected a finding, got clean"
	else
		ok "$name"
	fi
}

echo "makefile_check_test.sh"

# --- the defect the check exists for -------------------------------------
want_fail "a bare pipe in a recipe is a finding" \
	"$(printf 'demo:\n\t@go test -json ./... | fo --format llm')"

want_fail "a pipe on a CONTINUATION line is still the same shell" \
	"$(printf 'demo:\n\t@echo start; \\\\\n\t\tgo test -json ./... | fo --format llm')"

want_fail "a pipe inside an if/else branch counts" \
	"$(printf 'dupl:\n\t@if ! command -v jscpd >/dev/null; then \\\\\n\t\techo skip; \\\\\n\telse \\\\\n\t\tfo wrap jscpd <r.json | fo --format llm; \\\\\n\tfi')"

# --- the exemptions ------------------------------------------------------
want_pass "set -o pipefail on the line clears it" \
	"$(printf 'demo:\n\t@set -o pipefail; go test -json ./... | fo --format llm')"

want_pass "set -euo pipefail also clears it" \
	"$(printf 'demo:\n\t@set -euo pipefail; go test -json ./... | fo --format llm')"

want_pass "a line ending in || true is opting out on purpose" \
	"$(printf 'report:\n\t@( $(CMD) ) | fo --format llm || true')"

want_pass "the make-strict marker clears it" \
	"$(printf 'x:\n\t@a | b   # make-strict: ok — b is advisory')"

# --- the shapes that must NOT be mistaken for a pipe ---------------------
# Regression: bash 5.2 reads `&` in a ${//} replacement as the matched text,
# so blanking `||` with `&&` doubled it into `||||` and every fallback below
# read as two pipes.
want_pass "|| is a fallback, not a pipe" \
	"$(printf 'deploy:\n\t@echo "=== deployed ($$(loto version 2>/dev/null || echo unknown)) ==="')"

want_pass "a | inside a single-quoted jq filter is not a pipe" \
	"$(printf 'arch:\n\t@jq -e .ok f.json >/dev/null || { \\\\\n\t\tjq %s.Payload | {A, B}%s f.json; \\\\\n\t\texit 1; \\\\\n\t}' "'" "'")"

want_pass "an apostrophe inside a double-quoted string does not unbalance the scan" \
	"$(printf 'arch:\n\t@jq -e .ok f.json >/dev/null || { \\\\\n\t\techo "fo%ss wrapper drops warnings"; \\\\\n\t\tjq %s.Payload | {A}%s f.json; \\\\\n\t\texit 1; \\\\\n\t}' "'" "'" "'")"

want_pass "a variable assignment's continuations are not recipe lines" \
	"$(printf 'REPORT_CMD = set +e; \\\\\n\techo one; \\\\\n\tgo build ./... | fo wrap diag; \\\\\n\techo two\n\nx:\n\t@echo hi')"

want_pass "a comment line holding a pipe is not a recipe" \
	"$(printf '# a | b would be a finding if this were a recipe\nx:\n\t@echo hi')"

want_pass "no recipes at all is clean, not a crash" \
	"$(printf 'VAR := 1\n')"

# --- the real thing ------------------------------------------------------
if out=$(bash "$checker" "$repo_root/Makefile" 2>&1); then
	ok "the repo's own Makefile passes"
else
	bad "the repo's own Makefile passes" "$out"
fi

# --- usage ---------------------------------------------------------------
if bash "$checker" "$tmp/does-not-exist" >/dev/null 2>&1; then
	bad "a missing Makefile is a usage error"
else
	rc=$?
	if [ "$rc" -eq 2 ]; then
		ok "a missing Makefile is a usage error (exit 2, not a finding)"
	else
		bad "a missing Makefile is a usage error" "exit=$rc want 2"
	fi
fi

echo
if [ "$fails" -eq 0 ]; then
	echo "✓ makefile_check_test.sh: $ran checks passed"
	exit 0
fi
echo "✗ makefile_check_test.sh: $fails of $ran checks failed"
exit 1
