package cli

import (
	"bytes"
	"strings"
	"testing"

	"loto/internal/lane"
)

// TestVerify_PassingCommand wraps lane.Verify: a zero-exit command against a
// real commit reports passed and exits 0.
func TestVerify_PassingCommand(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdVerify, tcHEAD, "--", "sh", "-c", tcShExit0}, &out, &errB)
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✓ verify passed ") {
		t.Errorf("missing pass triage line: %q", out.String())
	}
}

// TestVerify_FailingCommand maps a non-zero command exit to a test failure
// (exit 1), not an infra error.
func TestVerify_FailingCommand(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdVerify, tcHEAD, "--", "sh", "-c", "exit 3"}, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✗ verify failed ") {
		t.Errorf("missing fail triage line: %q", out.String())
	}
}

// TestVerify_SurfacesCommandOutput proves the wrapped command's stdout reaches
// the caller.
func TestVerify_SurfacesCommandOutput(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdVerify, tcHEAD, "--", "sh", "-c", "echo HELLO_FROM_CMD"}, &out, &errB)
	if code != 0 {
		t.Fatalf("want exit 0, got %d; err=%q", code, errB.String())
	}
	if !strings.Contains(out.String(), "HELLO_FROM_CMD") {
		t.Errorf("verify did not surface command output: %q", out.String())
	}
}

// TestVerify_NoCommand_UsageError rejects an invocation with a commit but no
// command to run.
func TestVerify_NoCommand_UsageError(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdVerify, tcHEAD}, &out, &errB)
	if code != 2 {
		t.Fatalf("want exit 2, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
}

// TestVerify_ReportsWhichTreeRan pins the tree= row. It is the only handle a
// caller has on whether the reused verify worktree is working: a repo that has
// silently fallen back to cutting one per verify pays seconds per promotion and
// is otherwise indistinguishable from the fast path.
func TestVerify_ReportsWhichTreeRan(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")

	var out, errB bytes.Buffer
	if code := Run([]string{tcCmdVerify, tcHEAD, "--", "sh", "-c", tcShExit0}, &out, &errB); code != 0 {
		t.Fatalf("want exit 0, got %d; err=%q", code, errB.String())
	}
	// The first verify in a repo has nothing to reuse, so it cuts the tree and
	// says so in the ⚠ advisory form, with a reason.
	if !strings.Contains(out.String(), "⚠ verify tree=recut reason=") {
		t.Errorf("first verify does not report its tree: %q", out.String())
	}

	out.Reset()
	errB.Reset()
	if code := Run([]string{tcCmdVerify, tcHEAD, "--", "sh", "-c", tcShExit0}, &out, &errB); code != 0 {
		t.Fatalf("want exit 0, got %d; err=%q", code, errB.String())
	}
	if !strings.Contains(out.String(), "ℹ verify tree=reuse\n") {
		t.Errorf("second verify does not report the reuse fast path: %q", out.String())
	}
	if strings.Contains(out.String(), "reason=") {
		t.Errorf("the fast path carries a reason: %q", out.String())
	}
}

// TestVerifyTreeRow is the row's own table: glyph by mode, reason only when
// present, and silence for a VerifyResult that predates the field.
func TestVerifyTreeRow(t *testing.T) {
	tests := []struct {
		name   string
		mode   lane.VerifyTreeMode
		reason string
		want   string
	}{
		{"reuse is neutral data", lane.TreeReuse, "", "ℹ verify tree=reuse"},
		{"recut is advisory", lane.TreeRecut, "poisoned", "⚠ verify tree=recut reason=poisoned"},
		{"fresh is advisory", lane.TreeFresh, "locked", "⚠ verify tree=fresh reason=locked"},
		{"no mode, no row", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyTreeRow(tc.mode, tc.reason); got != tc.want {
				t.Errorf("verifyTreeRow(%q, %q) = %q, want %q", tc.mode, tc.reason, got, tc.want)
			}
		})
	}
}
