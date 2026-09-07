#!/usr/bin/env bash
#
# githooks_chain_test.sh — drive .githooks/lib/run-chain.sh against fixture
# chains.
#
# Guards the contract downstream hooks code against (loto-ea8y.1): byte-order
# execution, fail-fast with the entry's own status, non-executable and parked
# entries skipped, argv passed through verbatim, stdin replayed to EVERY entry
# on a stdin-carrying hook, LOTO_GIT_HOOK exported.
#
# The stdin leg is the one worth a test: the old flat pre-push handed git's ref
# lines to a single reader, so a chain that forwarded the live pipe would let
# the first entry drain it and starve the rest.
#
# Run: make scriptcheck   (or: bash scripts/githooks_chain_test.sh)

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
runner_src=$repo/.githooks/lib/run-chain.sh
fails=0
ran=0

if [ ! -x "$runner_src" ]; then
	echo "✗ githooks_chain_test.sh: no executable $runner_src" >&2
	exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/loto-chain-test.XXXXXX") || {
	echo "✗ githooks_chain_test.sh: mktemp failed" >&2
	exit 2
}
trap 'rm -rf "$tmp"' EXIT

# A throwaway .githooks/ that uses the REAL runner, so this cannot drift from
# what ships. Entries are written per-case under $gh/hooks.d/<hook>/.
gh=$tmp/.githooks
mkdir -p "$gh/lib"
cp "$runner_src" "$gh/lib/run-chain.sh"
chmod +x "$gh/lib/run-chain.sh"
runner=$gh/lib/run-chain.sh

reset_chain() {
	rm -rf "$gh/hooks.d"
	mkdir -p "$gh/hooks.d/$1"
}

# entry <hook> <name> <body...>
entry() {
	local hook=$1 name=$2
	shift 2
	mkdir -p "$gh/hooks.d/$hook"
	{
		echo '#!/usr/bin/env sh'
		printf '%s\n' "$@"
	} >"$gh/hooks.d/$hook/$name"
	chmod +x "$gh/hooks.d/$hook/$name"
}

# check <name> <want-status> <want-output>  (output compared exactly)
check() {
	local name=$1 want_status=$2 want=$3 got status
	ran=$((ran + 1))
	got=$4
	status=$5
	if [ "$status" -ne "$want_status" ]; then
		printf '✗ %s: exit=%d want=%d\n' "$name" "$status" "$want_status"
		fails=$((fails + 1))
		return
	fi
	if [ "$got" != "$want" ]; then
		printf '✗ %s: output\n  got:  %q\n  want: %q\n' "$name" "$got" "$want"
		fails=$((fails + 1))
		return
	fi
	printf '✓ %s\n' "$name"
}

echo "githooks_chain_test.sh"

# ── name order is byte order, not the order they were created ────────────
reset_chain pre-commit
entry pre-commit 90-last 'echo c'
entry pre-commit 10-first 'echo a'
entry pre-commit 50-middle 'echo b'
out=$("$runner" pre-commit 2>&1)
check name-order-is-byte-order 0 $'a\nb\nc' "$out" $?

# ── first non-zero stops the chain and becomes the dispatcher's status ────
reset_chain pre-commit
entry pre-commit 10-ok 'echo ran-10'
entry pre-commit 20-fail 'echo ran-20; exit 7'
entry pre-commit 50-never 'echo ran-50'
out=$("$runner" pre-commit 2>/dev/null)
check fail-fast-status-is-the-entry-status 7 $'ran-10\nran-20' "$out" $?

reset_chain pre-commit
entry pre-commit 20-fail 'exit 7'
err=$("$runner" pre-commit 2>&1 >/dev/null)
check fail-fast-names-the-entry 7 '✗ pre-commit: hooks.d/pre-commit/20-fail exit=7 — chain stopped' "$err" $?

# ── a file the runner must not execute ───────────────────────────────────
reset_chain pre-commit
entry pre-commit 10-inert 'echo inert'
chmod -x "$gh/hooks.d/pre-commit/10-inert"
entry pre-commit 20-parked.disabled 'echo parked'
entry pre-commit 30-backup.orig 'echo backup'
entry pre-commit 40-live 'echo live'
out=$("$runner" pre-commit 2>&1)
check skips-inert-and-parked-entries 0 'live' "$out" $?

# ── argv reaches every entry verbatim ────────────────────────────────────
reset_chain prepare-commit-msg
entry prepare-commit-msg 10-argv 'echo "$#:$1:$2"'
out=$("$runner" prepare-commit-msg .git/COMMIT_EDITMSG message 2>&1)
check argv-passed-through 0 '2:.git/COMMIT_EDITMSG:message' "$out" $?

# ── stdin: replayed from the top to EVERY entry on a stdin-carrying hook ──
reset_chain pre-push
entry pre-push 10-reader 'echo "10 saw: $(cat)"'
entry pre-push 20-reader 'echo "20 saw: $(cat)"'
out=$(printf 'refs/heads/main abc refs/heads/main def\n' | "$runner" pre-push origin git@host:r 2>&1)
check stdin-replayed-to-every-entry 0 \
	$'10 saw: refs/heads/main abc refs/heads/main def\n20 saw: refs/heads/main abc refs/heads/main def' \
	"$out" $?

# ── stdin: a hook git feeds nothing gets /dev/null, never the terminal ────
reset_chain pre-commit
entry pre-commit 10-reader 'echo "saw:[$(cat)]"'
out=$("$runner" pre-commit </dev/null 2>&1)
check stdin-is-devnull-for-plain-hooks 0 'saw:[]' "$out" $?

# ── the event name reaches entries without parsing $0 ────────────────────
reset_chain post-merge
entry post-merge 10-env 'echo "$LOTO_GIT_HOOK"'
out=$("$runner" post-merge 2>&1)
check exports-loto-git-hook 0 'post-merge' "$out" $?

# ── a hook with no chain dir, and one with an empty dir, are both no-ops ──
reset_chain pre-commit
out=$("$runner" post-checkout 2>&1)
check missing-chain-dir-is-a-noop 0 '' "$out" $?

out=$("$runner" pre-commit 2>&1)
check empty-chain-dir-is-a-noop 0 '' "$out" $?

# ── every shipped dispatcher delegates here and has a chain dir ───────────
for h in pre-commit post-merge pre-push post-checkout prepare-commit-msg; do
	ran=$((ran + 1))
	if [ ! -x "$repo/.githooks/$h" ]; then
		printf '✗ shipped-%s-is-executable\n' "$h"
		fails=$((fails + 1))
		continue
	fi
	if ! grep -q 'lib/run-chain.sh" '"$h" "$repo/.githooks/$h"; then
		printf '✗ shipped-%s-delegates-to-run-chain\n' "$h"
		fails=$((fails + 1))
		continue
	fi
	if [ ! -d "$repo/.githooks/hooks.d/$h" ]; then
		printf '✗ shipped-%s-has-a-chain-dir\n' "$h"
		fails=$((fails + 1))
		continue
	fi
	printf '✓ shipped-%s-wired\n' "$h"
done

if [ "$fails" -ne 0 ]; then
	printf '\n✗ run-chain.sh: %d of %d checks failed\n' "$fails" "$ran"
	exit 1
fi
printf '\n✓ run-chain.sh: %d checks passed\n' "$ran"
