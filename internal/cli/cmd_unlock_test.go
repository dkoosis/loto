package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	code := Run([]string{tcCmdUnlock, tcTargetA, "--force"}, &out, &errBuf)
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
func TestUnlock_ForceRestoresWriteMode(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	p := filepath.Join(repo, tcTargetA)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock")
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected stripped after lock, got %o", st.Mode().Perm())
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcTargetA, "--force", "-t", "stuck"}, &out, &errBuf); code != 0 {
		t.Fatalf("force unlock exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	st, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored after break, got %o", st.Mode().Perm())
	}
}

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
