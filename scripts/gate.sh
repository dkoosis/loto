#!/usr/bin/env bash
#
# gate.sh — run a QA producer, render it through fo, and never report
# "no findings" when the producer never ran.
#
# loto-fcbp: fo exits 0 and prints "+ no findings" whether a tool genuinely
# found nothing or failed to start. The Makefile's pipefail still failed the
# gate, so `make check` was correct — and told you the opposite of the cause.
# An external reviewer lost a session to it: `make check` reported a clean vet
# and a non-zero exit, and only a hand-run `go vet` revealed a toolchain
# mismatch.
#
# Contract: fo's summary owns the normal path. When the producer exits
# non-zero AND nothing parsed out of its output, the raw diagnostic wins
# instead — with the tool named and the producer's exit status preserved.
#
# usage: gate.sh <tool> <diag|sarif|testjson> -- <producer> [args...]
#
#   diag      producer writes plain line diagnostics (go vet, nilaway).
#             stdout+stderr merge into one stream, wrapped to SARIF by fo.
#   sarif     producer writes SARIF on stdout (golangci-lint, govulncheck).
#             stderr is held back — shown only when the run fails empty.
#   testjson  producer writes `go test -json` on stdout.

set -uo pipefail

die() { printf 'gate.sh: %s\n' "$1" >&2; exit 2; }

[ $# -ge 4 ] || die "usage: gate.sh <tool> <diag|sarif|testjson> -- <producer> [args...]"

tool=$1
mode=$2
shift 2
[ "${1:-}" = "--" ] || die "expected -- before the producer command"
shift
[ $# -ge 1 ] || die "no producer command given"

case "$mode" in
	diag | sarif | testjson) ;;
	*) die "unknown mode: $mode" ;;
esac

# sarif_line prints the first line of a stream that parses as a SARIF document,
# and nothing at all when no line does. It exists because a producer may print
# a human summary after the document on the same stream — see the sarif case
# below for the measured shape. Scanning rather than taking line 1 blindly
# keeps a producer that prefixes a warning line working too.
sarif_line() {
	while IFS= read -r line; do
		case "$line" in
		'{'*) printf '%s\n' "$line" | jq -e 'has("runs")' >/dev/null 2>&1 && {
			printf '%s\n' "$line"
			return 0
		} ;;
		esac
	done <"$1"
	return 0
}

tmp=$(mktemp -d) || die "mktemp failed"
trap 'rm -rf "$tmp"' EXIT
out="$tmp/out"
err="$tmp/err"
: >"$err"

# Run the producer. `set -e` is deliberately off: a non-zero exit is the
# signal this wrapper exists to interpret, not a reason to abort.
if [ "$mode" = diag ]; then
	"$@" >"$out" 2>&1
	status=$?
else
	"$@" >"$out" 2>"$err"
	status=$?
fi

# Count what the render would actually show. A producer that died before
# emitting anything parseable counts as zero — including the case where its
# output is not valid JSON at all, since jq then fails and we fall back to 0.
render_input=$out
case "$mode" in
diag)
	fo wrap diag --tool "$tool" --level error <"$out" >"$tmp/sarif" 2>/dev/null || : >"$tmp/sarif"
	render_input=$tmp/sarif
	findings=$(jq '[.runs[]?.results[]?] | length' "$tmp/sarif" 2>/dev/null) || findings=0
	;;
sarif)
	# ‡ A SARIF producer may append a human summary to the same stream
	# (loto-36q8). golangci-lint does, unconditionally: with
	# --output.sarif.path=/dev/stdout it writes the document and then
	#
	#   2 issues:
	#   * modernize: 1
	#
	# after it. That trailer is not JSON, so jq failed on the whole stream,
	# findings fell back to 0, and the status!=0 branch below declared the tool
	# never ran — for EVERY real lint finding, naming no file and no rule. It
	# went unnoticed because a clean tree never reaches it. Not the text
	# formatter (--output.text.path=stderr does not move it) and not the
	# lint-locked wrapper: reproduced with the pinned binary run bare.
	#
	# The document is one line, so keeping the first line that parses as SARIF
	# is enough, and it is what the renderer gets too — cleaning the stream only
	# for the count would still hand `fo` a malformed document. A producer whose
	# whole output is already clean SARIF is unaffected: line one is the
	# document.
	sarif_line "$out" >"$tmp/sarif"
	render_input=$tmp/sarif
	findings=$(jq '[.runs[]?.results[]?] | length' "$tmp/sarif" 2>/dev/null) || findings=0
	;;
testjson)
	findings=$(grep -c '"Action":"fail"' "$out" || true)
	;;
esac
[ -n "${findings:-}" ] || findings=0

if [ "$status" -ne 0 ] && [ "$findings" -eq 0 ]; then
	printf '✗ %s did not run — exit=%d findings=0\n' "$tool" "$status"
	printf 'ℹ the tool failed before producing findings; raw output follows\n\n'
	diag=$err
	[ -s "$diag" ] || diag=$out
	if [ -s "$diag" ]; then
		tail -n 40 "$diag" | sed 's/^/  /'
	else
		printf '  (producer wrote nothing to stdout or stderr)\n'
	fi
	printf '\n'
	exit "$status"
fi

fo --format llm <"$render_input"
exit "$status"
