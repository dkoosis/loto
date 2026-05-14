package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
