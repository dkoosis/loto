package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestUnlockAll_NoPinnedIdentity_RefusesFalseSuccess pins the loto-pody
// regression: when neither LOTO_AGENT_ID nor CLAUDE_CODE_SESSION_ID is set,
// identity.Ensure mints a fresh UUID that owns zero locks, and the old
// unlockAll reported "0 locks released" (exit 0) — a silent false-success
// that left the caller's real locks in place with files write-stripped.
// The fix refuses with a pin-required diagnostic and exits non-zero.
func TestUnlockAll_NoPinnedIdentity_RefusesFalseSuccess(t *testing.T) {
	repo := withTempProject(t)
	// Seed a lock under a known agent so ReleaseBySession has rows to find —
	// if the bug is present the command returns exit 0 reporting 0 released,
	// which is the false-success we are guarding against.
	alice := pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}
	_ = alice
	_ = repo

	// Drop both pinning env vars so Ensure mints a brand-new throwaway UUID.
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("expected non-zero exit (pin-required refusal), got 0; stdout=%q stderr=%q",
			out.String(), errBuf.String())
	}
	errOut := errBuf.String()
	if !strings.Contains(errOut, "LOTO_AGENT_ID") {
		t.Errorf("diagnostic should mention LOTO_AGENT_ID; stderr=%q", errOut)
	}
}

// TestUnlockAll_BlankAgentID_RefusesFalseSuccess pins the loto-s3l blank-env
// vector: a set-but-EMPTY LOTO_AGENT_ID made agentIdentityPinned report pinned
// (LookupEnv set=true), but identity.Ensure treats explicit-empty as opting
// into a throwaway ephemeral agent that owns zero locks. release --all then
// scoped to that throwaway → released 0 → exit 0 false-success while the
// caller's real locks stayed put, files write-stripped. Post-alignment blank
// reads as unpinned → the write verb refuses before opening the store (exit 3,
// loto-jnid: every authority-bearing verb, not just --all).
func TestUnlockAll_BlankAgentID_RefusesFalseSuccess(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}

	t.Setenv("LOTO_AGENT_ID", "") // set-but-empty: Ensure mints a throwaway
	t.Setenv("LOTO_SUBAGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("expected exit 3 (unpinned refusal), got %d; stdout=%q stderr=%q",
			code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "LOTO_AGENT_ID") {
		t.Errorf("diagnostic should mention LOTO_AGENT_ID; stderr=%q", errBuf.String())
	}
}

// TestUnlockAll_MalformedSubagentID_RefusesFalseSuccess pins the aligned bonus
// vector: a traversal-shaped LOTO_SUBAGENT_ID is ignored by identity.Ensure
// (resolveSubagent falls open), so with no other identity env Ensure mints a
// throwaway. The pre-alignment `LOTO_SUBAGENT_ID != ""` pin let that throwaway
// scope release --all → the same silent false-success as the blank case. The
// shape-validated SubagentIDPins predicate refuses it (exit 3).
func TestUnlockAll_MalformedSubagentID_RefusesFalseSuccess(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}

	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOTO_SUBAGENT_ID", "../escape") // traversal-shaped: Ensure falls open

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("expected exit 3 (unpinned refusal), got %d; stdout=%q stderr=%q",
			code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "LOTO_AGENT_ID") {
		t.Errorf("diagnostic should mention LOTO_AGENT_ID; stderr=%q", errBuf.String())
	}
}

// TestUnlockAll_BlankAgentIDWithSessionSet_RefusesFalseSuccess pins the
// precedence edge (loto-s3l P1): identity.Ensure branches on LOTO_AGENT_ID
// set-ness BEFORE it consults CLAUDE_CODE_SESSION_ID, so a set-but-empty agent
// id mints a throwaway even when a session id is present — the session leg
// never rescues it. A flat `agentID != "" || session != ""` predicate would
// wrongly pin this combo (the one fleet dispatchers export) and re-open the
// false-success. The precedence-mirroring predicate refuses (exit 3).
func TestUnlockAll_BlankAgentIDWithSessionSet_RefusesFalseSuccess(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}

	t.Setenv("LOTO_SUBAGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "some-session") // present, but shadowed by the empty agent id
	t.Setenv("LOTO_AGENT_ID", "")                      // set-but-empty: Ensure mints a throwaway first

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("expected exit 3 (unpinned refusal), got %d; stdout=%q stderr=%q",
			code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "LOTO_AGENT_ID") {
		t.Errorf("diagnostic should mention LOTO_AGENT_ID; stderr=%q", errBuf.String())
	}
}

// TestUnlock_NoIntent_Succeeds pins loto-e0mz: plain unlock of your own lock
// must NOT require -t. ReleaseLocks takes no intent arg — the flag was validated
// then discarded, and the rejection landed on stderr (exit 2) while stdout (the
// Claude channel) stayed empty, reading to a subagent as a silent no-op that
// left the lock dangling. Releasing without -t must succeed and report it.
func TestUnlock_NoIntent_Succeeds(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("unlock without -t exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "✓ unlocked count=1") {
		t.Errorf("expected unlock success on stdout, got out=%q err=%q", out.String(), errBuf.String())
	}
}

// TestUnlock_ForceWithoutIntent_Rejected keeps -t required where it is actually
// consumed: --force feeds intent to BreakLocks for the break audit trail. Unlike
// plain unlock, breaking another agent's lock must still explain why.
func TestUnlock_ForceWithoutIntent_Rejected(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("force unlock without -t exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "-t required") {
		t.Errorf("expected -t required diagnostic, got %q", errBuf.String())
	}
}

// TestUnlock_ForceRestoresWriteMode confirms the break path (unlock --force)
// restores owner-write on the target file. Store layer already does this for
// owner-release; this test pins it through the CLI surface.
// TestUnlock_MultiTarget_BestEffortMissingVsNotOwner exercises render.EmitReleaseResults
// over a heterogeneous batch: one owned (unlock OK), one with no lock (no-lock row),
// one held by another agent (not-owner row → exit 1).
func TestUnlock_MultiTarget_BestEffortMissingVsNotOwner(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	for _, n := range []string{tcTargetB, tcTargetC} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock a")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcTargetC, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("bob lock c")
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcTargetB, tcTargetC, "-t", tcIntentDone}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "✓ unlocked count=1\n") {
		t.Errorf("triage: %q", got)
	}
	if !strings.Contains(got, "state=no-lock") {
		t.Errorf("missing no-lock: %q", got)
	}
	if !strings.Contains(got, "state=not-owner") {
		t.Errorf("missing not-owner: %q", got)
	}
}

// TestUnlock_StaleForeignLock_ReclaimedExit0 covers the D1 surface (loto-ebkc):
// a plain unlock of a target whose only holder is stale (TTL lapsed) reclaims
// it — exit 0, reclaimed row naming the dead owner, file writable again. The
// pre-D1 behavior bounced to ✗ not-owner/exit 1, whose only recourse was
// --force (wrong audit kind: lock_broken instead of lock_reclaimed_stale).
func TestUnlock_StaleForeignLock_ReclaimedExit0(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_PID", "") // PID-0 sentinel → TTL-only liveness

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	// D2 rejects non-positive TTLs: take the shortest lease, wait out expiry.
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}
	time.Sleep(20 * time.Millisecond) // lease expired → lock now stale

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("reclaiming unlock exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "✓ unlocked count=0 reclaimed=1\n") {
		t.Errorf("triage line must carry reclaimed=1: %q", got)
	}
	if !strings.Contains(got, "state=reclaimed-stale owner="+alice.UUID) {
		t.Errorf("reclaimed row must name the dead owner: %q", got)
	}
	st, err := os.Stat(filepath.Join(repo, tcTargetA))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("reclaim must restore owner-write, got %o", st.Mode().Perm())
	}
}

// TestUnlock_LiveForeignLock_StaysNotOwner pins the D1 regression edge at the
// CLI: a LIVE foreign lock still bounces a plain unlock to not-owner/exit 1.
func TestUnlock_LiveForeignLock_StaysNotOwner(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("unlock of live foreign lock exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	// not-owner row names the holder via holderTag (loto-a8t): the owner id.
	wantOwner := "owner=" + alice.UUID
	if !strings.Contains(out.String(), wantOwner) {
		t.Errorf("expected not-owner row naming alice as %q: %q", wantOwner, out.String())
	}
}

// TestUnlockForce_TagsDispossessedHolder is loto-sblu's central AC: alice
// holds a.go, bob force-breaks it, and a.go carries a territory tag naming
// bob and bob's intent by the time alice next reads that path — the lock row
// the note would otherwise hang off is gone the instant the break commits, so
// this is the one channel that survives it (D9, nug b2b0a9df507c).
func TestUnlockForce_TagsDispossessedHolder(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	intent := "loto-sblu: reclaim, alice unresponsive"
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", intent}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("force break exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ broken") {
		t.Fatalf("expected a broken row: %q", out.String())
	}

	// alice reads the path next and must see the note.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var check bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcTargetA}, &check, io.Discard); code != 0 {
		t.Fatalf("check exit %d: %q", code, check.String())
	}
	got := check.String()
	if !strings.Contains(got, "ℹ territory-tag") {
		t.Fatalf("expected a territory-tag row naming the break: %q", got)
	}
	if !strings.Contains(got, "from="+bob.UUID) {
		t.Errorf("tag must name the breaker: %q", got)
	}
	if !strings.Contains(got, intent) {
		t.Errorf("tag must carry the break's bead/intent: %q", got)
	}
	timeRe := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
	if !timeRe.MatchString(got) {
		t.Errorf("tag row must carry a time: %q", got)
	}
}

// TestUnlockForce_TagByteIdenticalAcrossReads is the golden-test requirement:
// unlike doctor's stash rows, a territory tag's fields (from=, prefix=,
// expires_at=, text=) are all fixed at write time, so two reads of the same
// note are byte-identical with no normalization needed.
func TestUnlockForce_TagByteIdenticalAcrossReads(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", "loto-sblu: reclaim"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("bob force break failed")
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var first, second bytes.Buffer
	Run([]string{tcCmdCheck, tcTargetA}, &first, io.Discard)
	Run([]string{tcCmdCheck, tcTargetA}, &second, io.Discard)
	if first.String() != second.String() {
		t.Errorf("the tag row must be byte-identical across reads:\n first  %q\n second %q", first.String(), second.String())
	}
}

// TestUnlockForce_NotifyFailure_WarnRowExitZero is Rule 3: a notification
// write that fails (here: the per-prefix territory-tag cap already exhausted)
// must not sink the break — it still succeeds, exit 0, with a ⚠ advisory row
// naming the failure instead of silently losing it.
func TestUnlockForce_NotifyFailure_WarnRowExitZero(t *testing.T) {
	withTempProject(t)
	alice, bob, carol := threeAgents(t)

	// Exhaust the per-prefix territory-tag cap on a.go while it is unlocked,
	// as a third voice, before alice ever takes the lock.
	t.Setenv("LOTO_AGENT_ID", carol.UUID)
	for i := range 5 {
		if code := Run([]string{tcCmdTag, tcTargetA, "loto-xyz: filler note"}, io.Discard, io.Discard); code != 0 {
			t.Fatalf("filler tag %d failed", i)
		}
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("a failed notification must not sink the break; exit %d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ broken") {
		t.Errorf("break must still succeed: %q", out.String())
	}
	if !strings.Contains(out.String(), "⚠ notify-failed") {
		t.Errorf("expected a ⚠ notify-failed row: %q", out.String())
	}
}

// TestUnlockForce_NoDispossession_NoTag: force-breaking a target nobody else
// holds (already stale, or self-owned) must not manufacture a note — the
// Rule is "dispossessed holder", not "every force break".
func TestUnlockForce_NoDispossession_NoTag(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	// No lock at all on tcTargetA: --force on an unlocked target is a
	// no-lock-at-target error, not a dispossession.
	var out, errBuf bytes.Buffer
	Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", tcIntentTest}, &out, &errBuf)

	var check bytes.Buffer
	Run([]string{tcCmdCheck, tcTargetA}, &check, io.Discard)
	if strings.Contains(check.String(), "territory-tag") {
		t.Errorf("no dispossession happened; no tag should exist: %q", check.String())
	}
}

// TestUnlockAll_OnlyIntent_ScopesToOneLane pins sd-xhap AC1: two lanes of one
// Claude Code session share an owner id (and, per loto-81n, can share a
// session id too), so a lane's own release must be scoped by something a
// lane controls per call — its lock-time intent — rather than by owner or
// session. Lane A holds A and B under "laneA: work"; lane B holds C under
// "laneB: work". Lane A's --only-intent release must take exactly A and B
// and leave C held, provable via `status --mine`.
func TestUnlockAll_OnlyIntent_ScopesToOneLane(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t) // one owner id for both "lanes" — the bug's precondition
	for _, n := range []string{tcTargetB, tcTargetC} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const laneAIntent = "laneA: work"
	const laneBIntent = "laneB: work"
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", laneAIntent}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lane A lock a: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", laneAIntent}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lane A lock b: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcTargetC, "-t", laneBIntent}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lane B lock c: exit %d", code)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, tcFlagOnlyIntent, laneAIntent}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("lane A scoped --all: exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "count=2") {
		t.Errorf("expected exactly A and B released (count=2); got %q", out.String())
	}

	var status bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &status, io.Discard); code != 0 {
		t.Fatalf("status --mine: exit %d", code)
	}
	got := status.String()
	if strings.Contains(got, tcTargetA) || strings.Contains(got, tcTargetB) {
		t.Errorf("A and B should be released after scoped --all; status: %q", got)
	}
	if !strings.Contains(got, tcTargetC) {
		t.Errorf("lane B's C must survive lane A's scoped --all; status: %q", got)
	}
}

// TestUnlockAll_OnlyIntent_NoMatch_ReleasesNothing: an --only-intent that
// matches none of the caller's own locks releases zero and leaves every lock
// standing — the filter fails closed, never falling back to a full sweep.
func TestUnlockAll_OnlyIntent_NoMatch_ReleasesNothing(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("seed lock exit %d", code)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, tcFlagOnlyIntent, "no-such-intent"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "no locks owned") {
		t.Errorf("expected a no-op report, got %q", out.String())
	}

	var status bytes.Buffer
	Run([]string{tcCmdStatus, tcFlagMine}, &status, io.Discard)
	if !strings.Contains(status.String(), tcTargetA) {
		t.Errorf("non-matching --only-intent must not release the seeded lock: %q", status.String())
	}
}

// TestUnlockAll_OnlyIntentWithoutAll_Rejected: --only-intent means nothing
// without --all — refuse rather than silently discard it (the same class of
// footgun loto-e0mz fixed for -t on plain unlock).
func TestUnlockAll_OnlyIntentWithoutAll_Rejected(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagOnlyIntent, "x"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), tcFlagOnlyIntent) {
		t.Errorf("diagnostic should name --only-intent: %q", errBuf.String())
	}
}

// TestUnlockAll_PeerIntents_WarnsThenReleases pins sd-xhap AC2: a plain,
// unfiltered `--all` whose own scope spans more than one intent — the signal
// that a shared owner id is carrying more than one lane's locks — prints the
// count and the distinct intents before releasing, then still releases
// everything (warn-and-proceed, not refuse: Rules allow either, and a
// deliberate full sweep across every lane is a legitimate call this repo
// still needs, e.g. genuine session end).
func TestUnlockAll_PeerIntents_WarnsThenReleases(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "laneA: work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock a: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", "laneB: work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock b: exit %d", code)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "⚠ unlock-all-peer-intents count=2") {
		t.Errorf("expected a peer-intents warning naming count=2, got %q", got)
	}
	if !strings.Contains(got, `"laneA: work"`) || !strings.Contains(got, `"laneB: work"`) {
		t.Errorf("warning should name both intents, got %q", got)
	}
	// Warn-and-proceed: both locks are still released.
	var status bytes.Buffer
	Run([]string{tcCmdStatus, tcFlagMine}, &status, io.Discard)
	if strings.Contains(status.String(), tcTargetA) || strings.Contains(status.String(), tcTargetB) {
		t.Errorf("full sweep should still release both after warning: %q", status.String())
	}
}

// TestUnlockAll_SingleIntent_NoWarning: the common case (one lane, one
// intent) must stay silent on the peer-intents front — no new noise for the
// well-behaved caller this bug was never about.
func TestUnlockAll_SingleIntent_NoWarning(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock a: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock b: exit %d", code)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", tcIntentDone}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "peer-intents") {
		t.Errorf("single-intent sweep must not warn: %q", out.String())
	}
}
