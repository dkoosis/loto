package lane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifytree_test.go is the executable spec for the reused verify worktree:
// one per repo under the git common dir, reset to the commit under test before
// each run, byte-identical to a fresh cut, and replaced by a throwaway on every
// path that cannot deliver it (missing tree, held lock).

// reuseTreePath is where the reused worktree must live for repoTop.
func reuseTreePath(t *testing.T, repoTop string) string {
	t.Helper()
	common, err := gitCommonDir(context.Background(), gitRunner{repoTop: repoTop}, repoTop)
	if err != nil {
		t.Fatalf("gitCommonDir: %v", err)
	}
	return filepath.Join(common, verifyTreeDir, verifyTreeLeaf)
}

// countRegistered is how many times git lists path as a worktree. Exactly one
// is the healthy answer for a live reuse tree; two would mean a re-cut left a
// stale admin entry behind.
func countRegistered(t *testing.T, repoTop, path string) int {
	t.Helper()
	want := canonPath(path)
	n := 0
	for line := range strings.SplitSeq(gitT(t, repoTop, "worktree", "list", "--porcelain"), "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok && canonPath(rest) == want {
			n++
		}
	}
	return n
}

// mustVerify runs Verify and fails on an infra error, returning the result.
func mustVerify(t *testing.T, repoTop, commit string, cmd ...string) VerifyResult {
	t.Helper()
	res, err := Verify(context.Background(), repoTop, commit, cmd)
	if err != nil {
		t.Fatalf("Verify infra error: %v\noutput:\n%s", err, res.Output)
	}
	return res
}

// TestVerifyReusesOneWorktreePerRepo: the first verify cuts the reuse tree
// under the git common dir, and every verify after it lands in the same one —
// never in the shared checkout, never a second registration.
func TestVerifyReusesOneWorktreePerRepo(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)

	if _, err := os.Stat(reuse); err == nil {
		t.Fatalf("setup: reuse tree already exists at %s", reuse)
	}

	for i := range 3 {
		res := mustVerify(t, repoTop, base, "sh", "-c", "exit 0")
		if !res.Passed {
			t.Fatalf("verify %d went RED on a no-op command:\n%s", i, res.Output)
		}
		if n := countRegistered(t, repoTop, reuse); n != 1 {
			t.Fatalf("after verify %d the reuse tree is registered %d times, want 1", i, n)
		}
	}

	// It lives under .git, so the shared checkout stays clean: a 327-file
	// sibling tree parked in the working tree would show up in every `ls`,
	// every editor tree and every `go build ./...` walk.
	if !strings.HasPrefix(canonPath(reuse), canonPath(filepath.Join(repoTop, ".git"))) {
		t.Errorf("reuse tree %q is not under the git dir", reuse)
	}
	if got := gitT(t, repoTop, "status", "--porcelain"); got != "" {
		t.Errorf("the shared checkout is dirty after verifies:\n%s", got)
	}
}

// TestVerifyReuseIsByteIdenticalToFreshCut is the load-bearing claim: reuse
// must not weaken the verify. A tree that has already served one verify and is
// then reset to a different commit holds exactly what `git worktree add
// --detach <that commit>` would have produced — same index (`ls-files -s`,
// mode + blob SHA + stage per path) and nothing else on disk.
func TestVerifyReuseIsByteIdenticalToFreshCut(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)
	tip := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))
	reuse := reuseTreePath(t, repoTop)

	// First verify at base cuts the tree; second at tip must RESET it — the
	// path this test exists to grade.
	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")
	mustVerify(t, repoTop, tip, "sh", "-c", "exit 0")

	fresh := filepath.Join(t.TempDir(), "fresh")
	gitT(t, repoTop, "worktree", "add", "--detach", fresh, tip)
	defer gitT(t, repoTop, "worktree", "remove", "--force", fresh)

	if got, want := gitT(t, reuse, "ls-files", "-s"), gitT(t, fresh, "ls-files", "-s"); got != want {
		t.Errorf("reused tree index differs from a fresh cut.\nreused:\n%s\nfresh:\n%s", got, want)
	}
	if got := gitT(t, reuse, "status", "--porcelain"); got != "" {
		t.Errorf("reused tree is not clean:\n%s", got)
	}
	if got := gitT(t, reuse, "rev-parse", "HEAD"); got != tip {
		t.Errorf("reused tree HEAD = %s, want the commit under verify %s", got, tip)
	}
	if got := gitT(t, reuse, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("reused tree is on branch %q; it must stay detached", got)
	}
}

// TestVerifyResetsDirtyReuseTree is the AC for a verify killed mid-run: it
// leaves build output, edits and deletions behind, and the NEXT verify must
// never see them. The probe runs inside the tree the command itself gets, so
// what it reports is what a real verify command would have seen.
func TestVerifyResetsDirtyReuseTree(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)

	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")

	// Every shape a killed verify leaves: an edited tracked file, a deleted
	// tracked file, an untracked file, an untracked dir, and an IGNORED build
	// artifact — the last is why the reset uses `clean -fdx` and not `-fd`.
	writeFile(t, reuse, "add.go", mulBroken)
	if err := os.Remove(filepath.Join(reuse, "mul.go")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, reuse, "stray.txt", "left by a killed verify\n")
	writeFile(t, reuse, "sub/dir/deep.txt", "and a whole tree of it\n")
	writeFile(t, reuse, ".gitignore", "junk/\n")
	writeFile(t, reuse, "junk/out.bin", "ignored build output\n")
	// A nested repo a test init'd and abandoned. `git clean -fdx` SKIPS this
	// one and says so; only -ff removes it — which is why the reset uses -ffdx.
	if err := os.MkdirAll(filepath.Join(reuse, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, filepath.Join(reuse, "nested"), "init", "-q", "-b", "main")

	// `git status --porcelain` names tracked drift and untracked files;
	// `git clean -ndx` names what is still there under an ignore rule. Both
	// silent = the command is looking at a tree indistinguishable from a fresh
	// cut.
	res := mustVerify(t, repoTop, base, "sh", "-c", "git status --porcelain; git clean -ndx")
	if !res.Passed {
		t.Fatalf("probe command failed:\n%s", res.Output)
	}
	if got := strings.TrimSpace(res.Output); got != "" {
		t.Errorf("the next verify saw a dirty tree; reset did not happen:\n%s", got)
	}
	if got := gitT(t, reuse, "rev-parse", "HEAD"); got != base {
		t.Errorf("reused tree HEAD = %s, want %s", got, base)
	}
}

// TestVerifyFreshCutWhenReuseTreeRemoved: the reuse tree is disposable. Deleted
// between promotions (a `rm -rf` on .git internals, a full-disk cleanup), the
// next verify re-cuts it in place and leaves exactly one registration — the
// stale one is cleared BY PATH, never by `git worktree prune`, which would reap
// a peer's in-flight worktree.
func TestVerifyFreshCutWhenReuseTreeRemoved(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)

	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")

	// A peer's orphaned worktree, in the same state `prune` would reap. Its
	// recorded path is captured BEFORE the dir goes: git stores the
	// symlink-resolved form (macOS /var -> /private/var) and a deleted path
	// can no longer be resolved to compare against it.
	sib := filepath.Join(t.TempDir(), "peer-wt")
	gitT(t, repoTop, "worktree", "add", "--detach", sib, base)
	sibRecorded := canonPath(sib)
	if err := os.RemoveAll(sib); err != nil {
		t.Fatal(err)
	}

	// The reuse tree's checkout is gone; its registration is now stale.
	if err := os.RemoveAll(reuse); err != nil {
		t.Fatal(err)
	}

	res := mustVerify(t, repoTop, base, "sh", "-c", "test -f add.go")
	if !res.Passed {
		t.Fatalf("verify after a removed reuse tree went RED:\n%s", res.Output)
	}
	if n := countRegistered(t, repoTop, reuse); n != 1 {
		t.Errorf("reuse tree registered %d times after a re-cut, want 1", n)
	}
	if after := gitT(t, repoTop, "worktree", "list", "--porcelain"); !strings.Contains(after, sibRecorded) {
		t.Errorf("the peer's orphaned worktree %q was reaped; a re-cut must remove only its own path:\n%s", sibRecorded, after)
	}
}

// TestVerifyFallsBackWhenReuseTreeLocked is the concurrency case, and it is a
// real one: gate.Promote runs phase 2 with NO promotion flock held, so two
// `loto promote` processes in one repo can be verifying at the same moment (as
// can any mix of `loto verify` and `loto lane --build`). The loser of the
// reuse-tree lock must cut its own worktree and finish, never wait and never
// share the tree the holder is running in.
func TestVerifyFallsBackWhenReuseTreeLocked(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)
	common := filepath.Dir(filepath.Dir(reuse))

	held, err := tryVerifyFlock(filepath.Join(common, verifyTreeLock))
	if err != nil {
		t.Fatalf("hold the reuse lock: %v", err)
	}

	res := mustVerify(t, repoTop, base, "sh", "-c", "test -f add.go")
	if !res.Passed {
		t.Fatalf("verify under a held reuse lock went RED:\n%s", res.Output)
	}
	if _, err := os.Stat(reuse); err == nil {
		t.Errorf("verify used the reuse tree while its lock was held by a peer")
	}
	if after := gitT(t, repoTop, "worktree", "list", "--porcelain"); strings.Contains(after, "loto-verify-") {
		t.Errorf("the fallback throwaway worktree was not torn down:\n%s", after)
	}

	// Released, the next verify goes back to reuse — the fallback is a
	// fallback, not a latch.
	held.release()
	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")
	if n := countRegistered(t, repoTop, reuse); n != 1 {
		t.Errorf("reuse tree registered %d times once the lock cleared, want 1", n)
	}
}
