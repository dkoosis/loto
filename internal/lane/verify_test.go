package lane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verify_test.go is the executable spec for Verify: a lane's broad-repo checks
// run in a throwaway, detached worktree cut off the lane ref — never against the
// shared dirty disk — with absolute worktree/git-dir paths scrubbed from the
// output and the worktree torn down BY PATH (never pruned). It reuses the
// lane_test.go harness (newBaseRepo, gitT, writeFile, laneOpts, mustCommit,
// addEdited, mulBroken) — same package.

// TestVerifyGreenAgainstLaneDespiteDirtyPeer is acceptance criterion (a): the
// lane's checks pass against base + only its own write-set, even while a peer's
// half-finished (non-compiling) edit sits on the shared disk. Proof of
// hermeticity — the ephemeral worktree forks from the lane COMMIT, so the broken
// on-disk mul.go never reaches the build.
func TestVerifyGreenAgainstLaneDespiteDirtyPeer(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited) // lane A: a valid edit
	writeFile(t, repoTop, "mul.go", mulBroken) // peer lane B: broken, on disk only

	tip := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))

	// Guard: the shared disk really is broken, so a non-hermetic verify (one that
	// built the working tree) would go RED — making GREEN a real isolation proof.
	if got, _ := os.ReadFile(filepath.Join(repoTop, "mul.go")); !strings.Contains(string(got), "undefinedHelper") {
		t.Fatalf("test setup: on-disk mul.go is not the broken peer edit")
	}

	res, err := Verify(context.Background(), repoTop, tip, []string{"go", "build", "./..."})
	if err != nil {
		t.Fatalf("Verify infra error: %v\noutput:\n%s", err, res.Output)
	}
	if !res.Passed {
		t.Errorf("lane verify went RED; expected GREEN off the lane ref.\noutput:\n%s", res.Output)
	}
}

// TestVerifyScrubsWorktreeAndGitDirPaths is acceptance criterion (b): output
// carries no absolute ephemeral-worktree path and no .git/.../worktrees/... path.
// go test -trimpath elides them for Go tooling; this exercises the backstop for
// non-Go tools by emitting both paths from a shell command.
func TestVerifyScrubsWorktreeAndGitDirPaths(t *testing.T) {
	repoTop, base := newBaseRepo(t)

	res, err := Verify(context.Background(), repoTop, base,
		[]string{"sh", "-c", "pwd; git rev-parse --absolute-git-dir"})
	if err != nil {
		t.Fatalf("Verify: %v\noutput:\n%s", err, res.Output)
	}
	if !res.Passed {
		t.Fatalf("probe command failed:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "/worktrees/") {
		t.Errorf("output leaked a .git/.../worktrees/ admin path:\n%s", res.Output)
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Output), "\n") {
		if strings.HasPrefix(line, "/") {
			t.Errorf("output leaked an absolute path %q\nfull output:\n%s", line, res.Output)
		}
	}
}

// TestVerifyRemovesWorktreeByPath is acceptance criterion (c): the ephemeral
// worktree is gone afterward — both its checkout dir and its git admin entry, so
// the porcelain worktree set is byte-identical before and after.
func TestVerifyRemovesWorktreeByPath(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	list := func() string { return gitT(t, repoTop, "worktree", "list", "--porcelain") }

	before := list()
	if _, err := Verify(context.Background(), repoTop, base, []string{"sh", "-c", "exit 0"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	after := list()

	if after != before {
		t.Errorf("worktree set changed; ephemeral worktree not removed.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if strings.Contains(after, "loto-verify") {
		t.Errorf("an ephemeral verify worktree survived:\n%s", after)
	}
}

// TestVerifyDoesNotPruneSiblingWorktrees is acceptance criterion (d): a
// concurrent peer's worktree — even one whose checkout dir has vanished
// (prunable) — survives our verify. Verify removes ONLY its own worktree by
// exact path and must NEVER `git worktree prune`, which would garbage-collect a
// peer's in-flight worktree mid-wave.
func TestVerifyDoesNotPruneSiblingWorktrees(t *testing.T) {
	repoTop, base := newBaseRepo(t)

	// A peer's concurrent verify worktree...
	sib := filepath.Join(t.TempDir(), "peer-wt")
	gitT(t, repoTop, "worktree", "add", "--detach", sib, base)

	// ...recorded by git under a canonicalized path; capture exactly what it stored.
	sibRecorded := ""
	for _, ln := range strings.Split(gitT(t, repoTop, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(ln, "worktree ") && strings.HasSuffix(ln, "peer-wt") {
			sibRecorded = strings.TrimPrefix(ln, "worktree ")
		}
	}
	if sibRecorded == "" {
		t.Fatalf("setup: peer worktree not listed")
	}

	// Orphan it: delete the checkout dir so its admin entry is now 'prunable' —
	// a `git worktree prune` anywhere would now reap it.
	if err := os.RemoveAll(sib); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(context.Background(), repoTop, base, []string{"sh", "-c", "exit 0"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	after := gitT(t, repoTop, "worktree", "list", "--porcelain")
	if !strings.Contains(after, sibRecorded) {
		t.Errorf("peer worktree %q was reaped by Verify; its admin entry must survive:\n%s", sibRecorded, after)
	}
}

// TestVerifyReportsCommandFailure: a non-zero command exit is a verify RESULT
// (Passed=false with output retained), not an infra error.
func TestVerifyReportsCommandFailure(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	res, err := Verify(context.Background(), repoTop, base,
		[]string{"sh", "-c", "echo stdout-line; echo stderr-line >&2; exit 3"})
	if err != nil {
		t.Fatalf("a non-zero command exit must not be an infra error: %v", err)
	}
	if res.Passed {
		t.Error("Passed=true for a command that exited 3")
	}
	if !strings.Contains(res.Output, "stdout-line") || !strings.Contains(res.Output, "stderr-line") {
		t.Errorf("combined stdout+stderr not both captured:\n%s", res.Output)
	}
}

func TestVerifyValidatesInput(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	cases := []struct {
		name    string
		repoTop string
		commit  string
		cmd     []string
	}{
		{"empty repoTop", "", base, []string{"true"}},
		{"empty commit", repoTop, "", []string{"true"}},
		{"nil cmd", repoTop, base, nil},
		{"empty cmd[0]", repoTop, base, []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(context.Background(), tc.repoTop, tc.commit, tc.cmd); !errors.Is(err, errVerifyInput) {
				t.Errorf("Verify(%s) = %v, want errVerifyInput", tc.name, err)
			}
		})
	}
}

func TestVerifyErrorsOnBadCommit(t *testing.T) {
	repoTop, _ := newBaseRepo(t)
	if _, err := Verify(context.Background(), repoTop,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", []string{"true"}); err == nil {
		t.Error("expected an error cutting a worktree off a nonexistent commit")
	}
}
