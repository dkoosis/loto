package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/domain"
)

// commitAllInRepo stages everything and records a commit in repo, returning its
// SHA. Lane commits fork off a real base commit, so tests must seed one.
func commitAllInRepo(t *testing.T, repo, msg string) string {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", tcHEAD)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// laneRefMessage returns the full commit message of refs/heads/loto/<ref>.
func laneRefMessage(t *testing.T, repo, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "log", "-1", "--format=%B", "refs/heads/loto/"+ref)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log %s: %v\n%s", ref, err, out)
	}
	return string(out)
}

func laneRefExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/loto/"+ref)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// TestLane_CommitsHeldWriteSet_CarriesClosesTrailer is the happy path: a write
// set the caller holds an exclusive lock on commits to the lane ref, and the
// recorded message carries the Closes: trailer the CLI built.
func TestLane_CommitsHeldWriteSet_CarriesClosesTrailer(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagRef, tcRefImpl1, tcFlagBase, base, "-m", "store: tweak", tcFlagCloses, tcClosesAbc}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✓ lane committed ") {
		t.Errorf("missing success triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "files=1") {
		t.Errorf("missing files count: %q", out.String())
	}
	msg := laneRefMessage(t, repo, tcRefImpl1)
	if !strings.Contains(msg, "Closes: loto-abc") {
		t.Errorf("commit message missing Closes trailer:\n%q", msg)
	}
}

// TestLane_NormalizesMultiCloses confirms several ids collapse into one
// canonical Closes: trailer.
func TestLane_NormalizesMultiCloses(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagRef, "impl-2", tcFlagBase, base, "-m", tcMsg, tcFlagCloses, "loto-abc, loto-def loto-abc"}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; err=%q", code, errB.String())
	}
	msg := laneRefMessage(t, repo, "impl-2")
	if !strings.Contains(msg, "Closes: loto-abc, loto-def") {
		t.Errorf("Closes trailer not normalized/deduped:\n%q", msg)
	}
}

// TestLane_RefusesWriteSetMissingLock is the core acceptance: loto lane refuses
// to commit a write set the caller does not hold every lock for, and writes no
// lane ref.
func TestLane_RefusesWriteSetMissingLock(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Lock only a.go; b.go is in the write set but unlocked.
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcTargetB, tcFlagRef, tcRefImpl1, tcFlagBase, base, "-m", tcMsg, tcFlagCloses, tcClosesAbc}, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✗ lane-blocked count=1 ") {
		t.Errorf("missing blocked triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "target="+tcTargetB) || !strings.Contains(out.String(), "reason=no-lock-held") {
		t.Errorf("missing %s no-lock-held row: %q", tcTargetB, out.String())
	}
	if laneRefExists(t, repo, tcRefImpl1) {
		t.Errorf("lane ref must not exist after a refused commit")
	}
}

// TestLane_PostAssertCatchesLostLock exercises the TOCTOU close: a lock dropped
// inside the stage window (a peer reclaim) is caught by the post-commit
// re-assertion, which reports the lane tainted.
func TestLane_PostAssertCatchesLostLock(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}

	// Simulate a peer reclaiming the lock between the pre-assert and staging by
	// releasing it through the command's own runtime store.
	laneAfterPreAssert = func(rt *runtime) {
		tgt, err := domain.Canonicalize(tcTargetA)
		if err != nil {
			t.Errorf("hook canonicalize: %v", err)
			return
		}
		if _, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{tgt}, domain.AgentUUID(rt.Agent.UUID)); err != nil {
			t.Errorf("hook release: %v", err)
		}
	}
	defer func() { laneAfterPreAssert = nil }()

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagRef, tcRefImpl1, tcFlagBase, base, "-m", tcMsg, tcFlagCloses, tcClosesAbc}, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1 (tainted), got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✗ lane-tainted ") {
		t.Errorf("missing tainted triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "reason=lock-lost") {
		t.Errorf("missing lock-lost reason: %q", out.String())
	}
}

// TestLane_RejectsSharedLock proves a shared (advisory) lock is not enough to
// commit — staging needs sole-writer (exclusive) ownership.
func TestLane_RejectsSharedLock(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("shared lock %s failed", tcTargetA)
	}
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagRef, tcRefImpl1, tcFlagBase, base, "-m", tcMsg, tcFlagCloses, tcClosesAbc}, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "reason=lock-not-exclusive") {
		t.Errorf("missing lock-not-exclusive reason: %q", out.String())
	}
}

// TestLane_MissingRef_UsageError rejects a malformed invocation with exit 2.
func TestLane_MissingRef_UsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagBase, tcHEAD, "-m", tcMsg, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 2 {
		t.Fatalf("want exit 2, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
}

// TestLane_EmptyWriteSet_UsageError rejects an invocation with no files.
func TestLane_EmptyWriteSet_UsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFlagRef, tcRefImpl1, tcFlagBase, tcHEAD, "-m", tcMsg, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 2 {
		t.Fatalf("want exit 2, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
}
