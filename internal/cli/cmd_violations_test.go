package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tcCmdViolations = "violations"
	tcSubScan       = "scan"
	tcSubResolve    = "resolve"
	tcBeadOvno9     = "loto-ovno.9"
)

// violationRepo is a committed project with refs/loto/integration pinned at
// HEAD — the baseline the sensor reads "differs from" against. Both standard
// targets are tracked, so either can be contaminated.
func violationRepo(t *testing.T) string {
	t.Helper()
	repo := withTempProject(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	submitGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	pinAgent(t)
	return repo
}

func rogueWrite(t *testing.T, repo, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runViolations(t *testing.T, args ...string) (code int, stdout string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(append([]string{tcCmdViolations}, args...), &out, &errBuf)
	return code, out.String()
}

// A clean tree reports an explicit ✓ count=0 header — silence from a
// contamination check is indistinguishable from a crash (design.md).
func TestViolations_CleanTreeReportsExplicitEmpty(t *testing.T) {
	violationRepo(t)

	code, out := runViolations(t, tcSubScan)
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, "✓ violations count=0") {
		t.Errorf("missing explicit empty header: %q", out)
	}
}

// The scripted demo from git-gate.md: a rogue write to an unleased path is
// recorded, and `loto violations` exits 1 so a wrapper can branch on it.
func TestViolations_RogueWriteToUnleasedPathIsRecorded(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")

	code, out := runViolations(t, tcSubScan)
	if code != 1 {
		t.Fatalf("want exit 1 with an open violation, got %d: %q", code, out)
	}
	if !strings.Contains(out, "✗ violations count=1") || !strings.Contains(out, "path="+tcTargetB) {
		t.Errorf("scan did not report the rogue write: %q", out)
	}
}

// The whole point of the sensor, end to end: contaminate an unleased path,
// then lock it and try to submit it. A valid lease must not launder the edit.
func TestViolations_ContaminatedPathCannotBeSubmitted(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")
	if code, out := runViolations(t, tcSubScan); code != 1 {
		t.Fatalf("seed scan: exit=%d out=%q", code, out)
	}

	// A perfectly ordinary lease, taken AFTER the contamination.
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetB, tcFlagBead, tcBeadOvno9}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want the candidate refused, got exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "reason=violation-intersect") {
		t.Errorf("want a violation-intersect rejection, got: %q", out.String())
	}
}

// The intersect must be an intersect: one dirty file elsewhere in the repo
// cannot freeze every submit.
func TestViolations_UnrelatedViolationDoesNotBlockASubmit(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")
	if code, out := runViolations(t, tcSubScan); code != 1 {
		t.Fatalf("seed scan: exit=%d out=%q", code, out)
	}

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	rogueWrite(t, repo, tcTargetA, "legitimate leased edit\n")

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno9}, &out, &errBuf); code != 0 {
		t.Fatalf("want accept — the violation is on another path: exit=%d out=%q err=%q",
			code, out.String(), errBuf.String())
	}
}

// A leaseholder's own edit is never a violation — the stated residual hole,
// asserted here through the real CLI so the store-level property cannot be
// true while the wiring quietly loses it.
func TestViolations_LeasedEditIsNotRecorded(t *testing.T) {
	repo := violationRepo(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	rogueWrite(t, repo, tcTargetA, "the holder's own work\n")

	code, out := runViolations(t, tcSubScan)
	if code != 0 {
		t.Fatalf("want a clean scan under a live lease, got exit=%d: %q", code, out)
	}
}

// Reverting the content resolves the violation without anyone saying so —
// otherwise the fix would look like it had not worked.
func TestViolations_RevertAutoResolves(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")
	if code, out := runViolations(t, tcSubScan); code != 1 {
		t.Fatalf("seed scan: exit=%d out=%q", code, out)
	}

	rogueWrite(t, repo, tcTargetB, "original\n")
	code, out := runViolations(t, tcSubScan)
	if code != 0 {
		t.Fatalf("want the revert to clear it, got exit=%d: %q", code, out)
	}
	if !strings.Contains(out, "resolved=1") {
		t.Errorf("scan did not report the auto-resolution: %q", out)
	}
}

// An explicit resolve clears a legitimate change and lets the submit through.
func TestViolations_ResolveUnblocksTheSubmit(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "vendored regen\n")
	_, scanOut := runViolations(t, tcSubScan)
	id := violationIDFromRow(t, scanOut)

	code, out := runViolations(t, tcSubResolve, id, "-m", "intentional regen")
	if code != 0 || !strings.Contains(out, "✓ violation-resolved id="+id) {
		t.Fatalf("resolve: exit=%d out=%q", code, out)
	}

	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock")
	}
	var sOut, sErr bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetB, tcFlagBead, tcBeadOvno9}, &sOut, &sErr); code != 0 {
		t.Fatalf("want accept after resolve: exit=%d out=%q err=%q", code, sOut.String(), sErr.String())
	}
}

// Resolving an id that names nothing open is a visible refusal, not a silent
// success: an operator who believes a violation is cleared and is wrong is
// worse off than one who sees the error.
func TestViolations_ResolveUnknownIDRefuses(t *testing.T) {
	violationRepo(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdViolations, tcSubResolve, "v-nosuchid"}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("want exit 3 for an unknown id, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "no unresolved violation") {
		t.Errorf("missing refusal row: %q", errBuf.String())
	}
}

// `loto status` resurfaces open violations without running the sensor —
// the read is cheap, the whole-tree diff is not, and status is what agents
// run to orient.
func TestViolations_StatusResurfacesTheNotice(t *testing.T) {
	repo := violationRepo(t)
	rogueWrite(t, repo, tcTargetB, "rogue\n")
	if code, out := runViolations(t, tcSubScan); code != 1 {
		t.Fatalf("seed scan: exit=%d out=%q", code, out)
	}

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &errBuf); code != 0 {
		t.Fatalf("status: exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "⚠ violations count=1") {
		t.Errorf("status did not resurface the violation: %q", out.String())
	}
}

// violationIDFromRow pulls the `id=` field out of a `✗ path=… id=…` row.
func violationIDFromRow(t *testing.T, out string) string {
	t.Helper()
	for field := range strings.FieldsSeq(out) {
		if strings.HasPrefix(field, "id=v-") {
			return strings.TrimPrefix(field, "id=")
		}
	}
	t.Fatalf("no violation id in output: %q", out)
	return ""
}
