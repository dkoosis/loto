package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/lane"
)

// lane_build_fail_test.go is the executable spec for codex #286 finding 2:
// the fix block a --build failure prints must never be a bare `git update-ref
// -d`, because resolveParent makes a lane's Nth wave a child of its (N-1)th —
// so refs/heads/loto/<ref> can carry earlier SUCCESSFUL waves the failed
// commit is merely stacked on top of. Two shapes: a first-wave failure (safe
// to delete, nothing preceded it) and a later-wave failure (must reset to the
// parent, preserving the prior wave).

// laneIDT is the fixed author/committer identity these tests build commits
// with — content doesn't matter, only that Commit has one.
var laneIDT = lane.Identity{Name: "t", Email: "t@t"} //nolint:gochecknoglobals // test fixture

// initGoModRepo creates a minimal git repo with a committed go.mod (git
// identity/hook isolation matches cmd_lock_test.go's initBareGitRepo) and
// returns its root and the base commit SHA.
func initGoModRepo(t *testing.T) (repoTop, base string) {
	t.Helper()
	repoTop = t.TempDir()
	initBareGitRepo(t, repoTop)
	if err := os.WriteFile(filepath.Join(repoTop, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoTop, commitAllInRepo(t, repoTop, "chore: init")
}

// TestLaneBuildFailRemediation_FirstWaveDeletes is shape (a): nothing
// preceded the failed commit on the ref (its parent equals Base), so the safe
// remediation is an outright — still CAS-guarded — delete.
func TestLaneBuildFailRemediation_FirstWaveDeletes(t *testing.T) {
	repoTop, base := initGoModRepo(t)

	commit, err := lane.Commit(context.Background(), lane.Opts{
		RepoTop: repoTop, Base: base, Ref: "wave1fail",
		WriteSet:  []string{"go.mod"},
		Message:   "loto/wave1fail\n\nCloses: none\n",
		Author:    laneIDT,
		Committer: laneIDT,
	})
	if err != nil {
		t.Fatalf("lane.Commit: %v", err)
	}

	got, err := laneBuildFailRemediation(context.Background(), repoTop, "wave1fail", base, commit)
	if err != nil {
		t.Fatalf("laneBuildFailRemediation: %v", err)
	}
	want := "git update-ref -d refs/heads/loto/wave1fail " + commit
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	assertGitUpdateRefRoundtrips(t, repoTop, got, "wave1fail", "")
}

// TestLaneBuildFailRemediation_LaterWaveResetsToParent is shape (b): a prior
// SUCCESSFUL wave already sits on the ref. The remediation must reset to that
// wave's SHA, never delete it.
func TestLaneBuildFailRemediation_LaterWaveResetsToParent(t *testing.T) {
	repoTop, base := initGoModRepo(t)

	wave1, err := lane.Commit(context.Background(), lane.Opts{
		RepoTop: repoTop, Base: base, Ref: "wave2fail",
		WriteSet:  []string{"go.mod"},
		Message:   "loto/wave2fail wave1\n\nCloses: none\n",
		Author:    laneIDT,
		Committer: laneIDT,
	})
	if err != nil {
		t.Fatalf("lane.Commit wave1: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repoTop, "extra.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wave2, err := lane.Commit(context.Background(), lane.Opts{
		RepoTop: repoTop, Base: base, Ref: "wave2fail", // same ref: resolveParent picks up wave1
		WriteSet:  []string{"extra.go"},
		Message:   "loto/wave2fail wave2 (build-fails)\n\nCloses: none\n",
		Author:    laneIDT,
		Committer: laneIDT,
	})
	if err != nil {
		t.Fatalf("lane.Commit wave2: %v", err)
	}

	got, err := laneBuildFailRemediation(context.Background(), repoTop, "wave2fail", base, wave2)
	if err != nil {
		t.Fatalf("laneBuildFailRemediation: %v", err)
	}
	want := fmt.Sprintf("git update-ref refs/heads/loto/wave2fail %s %s", wave1, wave2)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	assertGitUpdateRefRoundtrips(t, repoTop, got, "wave2fail", wave1)
}

// assertGitUpdateRefRoundtrips actually RUNS the printed remediation command
// against repoTop and checks the ref lands where the fix block promised —
// proving the command is not just correctly WORDED but genuinely safe to
// paste. wantAfter == "" means the ref must no longer resolve at all.
func assertGitUpdateRefRoundtrips(t *testing.T, repoTop, cmdLine, ref, wantAfter string) {
	t.Helper()
	fields := strings.Fields(cmdLine)
	c := exec.Command(fields[0], fields[1:]...)
	c.Dir = repoTop
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("running the printed remediation %q: %v\n%s", cmdLine, err, out)
	}
	refName := "refs/heads/loto/" + ref
	out, err := exec.Command("git", "-C", repoTop, "rev-parse", "--verify", "--quiet", refName).Output()
	if wantAfter == "" {
		if err == nil {
			t.Errorf("%s still resolves to %s after the fix block; want it gone", refName, strings.TrimSpace(string(out)))
		}
		return
	}
	if err != nil {
		t.Fatalf("%s no longer resolves after the fix block: %v", refName, err)
	}
	if got := strings.TrimSpace(string(out)); got != wantAfter {
		t.Errorf("%s = %s after the fix block, want %s", refName, got, wantAfter)
	}
}
