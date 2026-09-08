#!/usr/bin/env bash
#
# githooks_loto_entries_test.sh — golden tests for loto's own chain entries:
# .githooks/hooks.d/pre-commit/10-loto-staged-locks (loto-7oik) and
# .githooks/hooks.d/post-checkout/10-loto-moved-locks (loto-ea8y.2).
#
# The entries are shell, and what they get wrong is shell-shaped: which
# stream a refusal lands on, whether a warn-mode advisory survives an exit 0,
# whether a ✓ is turned into the silence a post-checkout hook owes. None of
# that is reachable from Go, so the golden lives here.
#
# `loto` is a stub on PATH driven by FAKE_*_OUT / FAKE_*_EXIT, so these
# exercise the ENTRIES against the CLI contract rather than the CLI itself —
# internal/cli/cmd_check_held_test.go and cmd_check_moved_test.go pin the
# other side of it.
#
# Run: make scriptcheck   (or: bash scripts/githooks_loto_entries_test.sh)

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
precommit=$repo/.githooks/hooks.d/pre-commit/10-loto-staged-locks
postcheckout=$repo/.githooks/hooks.d/post-checkout/10-loto-moved-locks
fails=0
ran=0

for entry in "$precommit" "$postcheckout"; do
	if [ ! -x "$entry" ]; then
		echo "✗ githooks_loto_entries_test.sh: $entry is missing or not executable" >&2
		exit 2
	fi
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/loto-entries-test.XXXXXX") || {
	echo "✗ githooks_loto_entries_test.sh: mktemp failed" >&2
	exit 2
}
trap 'rm -rf "$tmp"' EXIT

# A `loto` stub. Answers each leg from its own pair of env vars, so one case
# can make leg 1 pass and leg 2 refuse.
mkdir -p "$tmp/bin"
cat >"$tmp/bin/loto" <<'STUB'
#!/usr/bin/env sh
case "$*" in
*--gate*)
	[ -n "${FAKE_GATE_OUT:-}" ] && printf '%s\n' "$FAKE_GATE_OUT"
	exit "${FAKE_GATE_EXIT:-0}"
	;;
*--held*)
	[ -n "${FAKE_HELD_OUT:-}" ] && printf '%s\n' "$FAKE_HELD_OUT"
	exit "${FAKE_HELD_EXIT:-0}"
	;;
*--moved*)
	[ -n "${FAKE_MOVED_OUT:-}" ] && printf '%s\n' "$FAKE_MOVED_OUT"
	exit "${FAKE_MOVED_EXIT:-0}"
	;;
esac
exit 0
STUB
chmod +x "$tmp/bin/loto"

withloto=$tmp/bin:$PATH
# A PATH with no `loto` at all, for the fail-open leg.
noloto=/usr/bin:/bin

# check <name> <want-status> <want-stdout> <want-stderr> <got-status> <got-stdout> <got-stderr>
check() {
	local name=$1 want_status=$2 want_out=$3 want_err=$4 status=$5 got_out=$6 got_err=$7
	ran=$((ran + 1))
	if [ "$status" -ne "$want_status" ]; then
		printf '✗ %s: exit=%d want=%d\n' "$name" "$status" "$want_status"
		fails=$((fails + 1))
		return
	fi
	if [ "$got_out" != "$want_out" ]; then
		printf '✗ %s: stdout\n  got:  %q\n  want: %q\n' "$name" "$got_out" "$want_out"
		fails=$((fails + 1))
		return
	fi
	if [ "$got_err" != "$want_err" ]; then
		printf '✗ %s: stderr\n  got:  %q\n  want: %q\n' "$name" "$got_err" "$want_err"
		fails=$((fails + 1))
		return
	fi
	printf '✓ %s\n' "$name"
}

# run_entry <entry> <args...> — runs the entry with the exports currently in
# force and leaves its status/stdout/stderr in STATUS/OUT/ERR. No subshell:
# a case has to be able to bump the counters above.
run_entry() {
	local entry=$1
	shift
	sh "$entry" "$@" >"$tmp/out" 2>"$tmp/err"
	STATUS=$?
	OUT=$(cat "$tmp/out")
	ERR=$(cat "$tmp/err")
}

# reset_fakes clears every stub knob so a case only carries what it sets.
reset_fakes() {
	unset FAKE_GATE_OUT FAKE_GATE_EXIT FAKE_HELD_OUT FAKE_HELD_EXIT
	unset FAKE_MOVED_OUT FAKE_MOVED_EXIT LOTO_GUARD_OVERRIDE
	export PATH=$withloto
}

echo "githooks_loto_entries_test.sh"

# ══ post-checkout/10-loto-moved-locks ═════════════════════════════════════

# A clean move says ✓ on stdout; the entry turns that into silence, because a
# ✓ after every `git checkout` trains the eye past the ⚠ that matters.
reset_fakes
export FAKE_MOVED_OUT='✓ moved-peer-locks count=0' FAKE_MOVED_EXIT=0
run_entry "$postcheckout" abc123 def456 1
check moved-clean-checkout-is-silent 0 '' '' "$STATUS" "$OUT" "$ERR"

# A peer-held path reaches the mover on stderr, verbatim, and the checkout
# still succeeds.
moved_warn=$'⚠ moved-peer-locks count=1\n⚠ path=a.go kind=lock blocker=u1 intent="edit" expires_at=2026-09-07T00:00:00Z\nℹ options=tell-the-holder|move-back|carry-on — the move already happened'
reset_fakes
export FAKE_MOVED_OUT="$moved_warn" FAKE_MOVED_EXIT=0
run_entry "$postcheckout" abc123 def456 1
check moved-peer-lock-warns-on-stderr 0 '' "$moved_warn" "$STATUS" "$OUT" "$ERR"

# The advisory never turns a checkout into a failure, whatever loto says.
reset_fakes
export FAKE_MOVED_EXIT=3
run_entry "$postcheckout" abc123 def456 1
check moved-nonzero-loto-still-exits-0 0 '' '' "$STATUS" "$OUT" "$ERR"

reset_fakes
export PATH=$noloto
run_entry "$postcheckout" abc123 def456 1
check moved-fails-open-without-loto 0 '' '' "$STATUS" "$OUT" "$ERR"

reset_fakes
export LOTO_GUARD_OVERRIDE=1 FAKE_MOVED_OUT="$moved_warn"
run_entry "$postcheckout" abc123 def456 1
check moved-override-is-silent 0 '' '' "$STATUS" "$OUT" "$ERR"

# ══ pre-commit/10-loto-staged-locks ═══════════════════════════════════════

reset_fakes
export FAKE_GATE_EXIT=0 FAKE_HELD_OUT='✓ held count=2' FAKE_HELD_EXIT=0
run_entry "$precommit"
check precommit-both-legs-pass 0 '' '' "$STATUS" "$OUT" "$ERR"

# Leg 1 (a peer holds a staged path) refuses, and leg 2 never runs.
gate_deny=$'✗ blocked count=1\n✗ path=a.go kind=lock blocker=u1 intent="edit" expires_at=2026-09-07T00:00:00Z'
reset_fakes
export FAKE_GATE_OUT="$gate_deny" FAKE_GATE_EXIT=1 FAKE_HELD_OUT='never' FAKE_HELD_EXIT=1
run_entry "$precommit"
want=$gate_deny$'\npre-commit: refused — staged paths include a peer\'s live lock/claim\npre-commit: override with LOTO_GUARD_OVERRIDE=1 git commit ...'
check precommit-leg1-refusal-short-circuits 1 '' "$want" "$STATUS" "$OUT" "$ERR"

# Leg 2 (loto-7oik) under LOTO_GATE_MODE=block: a staged path this session
# does not hold. The entry has no opinion about which mode is the default —
# `loto check --held` owns that — so both legs of the fork are stubbed here by
# exit code, and internal/cli/cmd_check_held_test.go pins which one ships.
held_deny=$'✗ unheld count=1 unlocked=1 peer=0 staged=2\n✗ path=b.go state=unlocked\n```bash\nloto lock \'b.go\' -t "<bead>: intent"  # take what you are about to commit\n```'
reset_fakes
export FAKE_GATE_EXIT=0 FAKE_HELD_OUT="$held_deny" FAKE_HELD_EXIT=1
run_entry "$precommit"
want=$held_deny$'\npre-commit: refused — staged paths this session does not hold a lock on\npre-commit: override with LOTO_GUARD_OVERRIDE=1 git commit ...'
check precommit-leg2-refuses-unheld-paths 1 '' "$want" "$STATUS" "$OUT" "$ERR"

# The shipped default: leg 2 exits 0 with ⚠ rows. The commit proceeds AND the
# rows still reach the committer — a counter nobody can see would make the
# advisory period pointless.
held_warn=$'⚠ unheld count=1 unlocked=1 peer=0 staged=2\n⚠ path=b.go state=unlocked\n```bash\nloto lock \'b.go\' -t "<bead>: intent"  # take what you are about to commit\n```'
reset_fakes
export FAKE_GATE_EXIT=0 FAKE_HELD_OUT="$held_warn" FAKE_HELD_EXIT=0
run_entry "$precommit"
check precommit-warn-mode-advises-and-proceeds 0 '' "$held_warn" "$STATUS" "$OUT" "$ERR"

# A store loto cannot reach (exit 3) proceeds — the gate never becomes the
# outage.
reset_fakes
export FAKE_GATE_EXIT=0 FAKE_HELD_EXIT=3
run_entry "$precommit"
check precommit-fails-open-on-infra 0 '' '' "$STATUS" "$OUT" "$ERR"

reset_fakes
export PATH=$noloto
run_entry "$precommit"
check precommit-fails-open-without-loto 0 '' '' "$STATUS" "$OUT" "$ERR"

reset_fakes
export LOTO_GUARD_OVERRIDE=1 FAKE_GATE_EXIT=1 FAKE_HELD_EXIT=1
run_entry "$precommit"
check precommit-override-skips-both-legs 0 '' 'pre-commit: LOTO_GUARD_OVERRIDE=1 — skipping loto staged-lock guard' "$STATUS" "$OUT" "$ERR"

if [ "$fails" -ne 0 ]; then
	printf '\n✗ loto chain entries: %d of %d checks failed\n' "$fails" "$ran"
	exit 1
fi
printf '\n✓ loto chain entries: %d checks passed\n' "$ran"
