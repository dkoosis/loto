package lane

import (
	"context"
	"os"
	"os/exec"
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

// runGitT is gitT for a command expected to FAIL: it hands back the error
// instead of ending the test, so a guard can assert that git refuses.
func runGitT(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
		// The first cut is a recut (nothing to reuse yet); every one after it
		// must report the fast path, and reporting it is the whole handle a
		// caller has on whether reuse is working.
		want := TreeReuse
		if i == 0 {
			want = TreeRecut
		}
		if res.Tree != want {
			t.Errorf("verify %d ran tree=%q reason=%q, want %q", i, res.Tree, res.TreeReason, want)
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

// TestVerifyRecutsAPoisonedReuseTree is the regression for the review finding
// on #303: a checkout failure was read as a bad commit-ish, so a tree that
// refuses EVERY commit was never rebuilt and every later verify silently paid a
// full cut, forever, with no signal.
//
// The poison is two commands: `git update-index --skip-worktree <file>` tells
// git the worktree copy of that path is authoritative, and an edit under that
// flag makes `checkout --force` refuse every commit that would change it
// ("Entry 'x' not uptodate. Cannot merge."), while `clean` never touches a
// tracked file. It is also INVISIBLE to `git status --porcelain`, so the
// post-reset clean check cannot catch it either — a re-cut is the only exit,
// and the worktree's private index is where the flag dies.
//
// ‡ Both `--assume-unchanged` and `--skip-worktree` defeat `checkout --force`
// on git 2.55.0 ("Entry not uptodate. Cannot merge.", measured twice, 2026-09-05).
// The test pins `--skip-worktree` because it is the harder case (status stays
// silent). The fix is the same either way: it turns on whether the COMMIT
// resolves, never on which local flag made the checkout fail.
func TestVerifyRecutsAPoisonedReuseTree(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	writeFile(t, repoTop, "add.go", addEdited)
	tip := mustCommit(t, laneOpts(repoTop, base, "A", "add.go"))
	reuse := reuseTreePath(t, repoTop)

	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")

	gitT(t, reuse, "update-index", "--skip-worktree", "add.go")
	writeFile(t, reuse, "add.go", mulBroken)

	// Guard: the poison really does defeat both of the reset's own commands,
	// and hides from the clean check — so a passing verify below can only mean
	// the tree was rebuilt.
	gitT(t, reuse, "clean", "-ffdx")
	if out, err := runGitT(reuse, "checkout", "--force", "--detach", tip); err == nil {
		t.Fatalf("setup: poisoned tree still accepts a checkout (%s)", out)
	}
	if got := gitT(t, reuse, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: expected the poison to be invisible to status, got %q", got)
	}

	// Verify at TIP, which the poisoned tree cannot check out — the reviewer's
	// exact path, and the one the old code misfiled as a bad commit-ish.
	res := mustVerify(t, repoTop, tip, "sh", "-c", "git status --porcelain")
	if !res.Passed {
		t.Fatalf("verify on a poisoned reuse tree went RED:\n%s", res.Output)
	}
	if res.Tree != TreeRecut {
		t.Errorf("tree=%q, want %q — a poisoned tree must be rebuilt, not routed around", res.Tree, TreeRecut)
	}
	if res.TreeReason == "" {
		t.Errorf("tree=recut carries no reason; a fallback with no signal is the bug being fixed")
	}
	if got := strings.TrimSpace(res.Output); got != "" {
		t.Errorf("the rebuilt tree is not clean:\n%s", got)
	}
	if n := countRegistered(t, repoTop, reuse); n != 1 {
		t.Errorf("reuse tree registered %d times after the re-cut, want 1", n)
	}
	if got, _ := os.ReadFile(filepath.Join(reuse, "add.go")); string(got) != addEdited {
		t.Errorf("add.go in the rebuilt tree is not the commit's content:\n%s", got)
	}

	// The poison is GONE, not merely stepped over: the next verify is back on
	// the fast path and the tree takes a checkout again.
	next := mustVerify(t, repoTop, base, "sh", "-c", "exit 0")
	if next.Tree != TreeReuse {
		t.Errorf("the verify after a re-cut ran tree=%q reason=%q, want %q",
			next.Tree, next.TreeReason, TreeReuse)
	}
	if next.TreeReason != "" {
		t.Errorf("tree=reuse carries reason=%q, want empty", next.TreeReason)
	}
	if _, err := runGitT(reuse, "checkout", gitForce, gitDetach, tip); err != nil {
		t.Errorf("the rebuilt tree still refuses a checkout; the poison survived")
	}
}

// TestVerifyRecutsWhenAnIndexFlagHidesDrift is the harder half of the same
// poison, and it is why the reset does not stop at `git status --porcelain`.
//
// When the tree already sits on the commit under verify, `checkout --force` is
// a no-op and SUCCEEDS — it never touches the flagged path — and status stays
// silent because that is what skip-worktree means. Both of the reset's own
// commands report success while the command would be handed a file the commit
// does not contain: reuse silently weakening the verify, the one outcome the
// bead forbids. `git ls-files -v` is the question that catches it.
func TestVerifyRecutsWhenAnIndexFlagHidesDrift(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)

	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")

	gitT(t, reuse, "update-index", "--skip-worktree", "add.go")
	writeFile(t, reuse, "add.go", mulBroken)

	// Guard: both of the reset's own commands are happy with the poison.
	gitT(t, reuse, "clean", "-ffdx")
	if out, err := runGitT(reuse, "checkout", gitForce, gitDetach, base); err != nil {
		t.Fatalf("setup: expected a same-commit checkout to succeed, got %v (%s)", err, out)
	}
	if got := gitT(t, reuse, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: expected the poison to be invisible to status, got %q", got)
	}

	// cat the file the poison replaced: what the verify COMMAND sees is the
	// only claim that matters.
	res := mustVerify(t, repoTop, base, "cat", "add.go")
	if !res.Passed {
		t.Fatalf("verify went RED:\n%s", res.Output)
	}
	if res.Tree != TreeRecut {
		t.Errorf("tree=%q, want %q — drift hidden by an index flag must still force a rebuild", res.Tree, TreeRecut)
	}
	if res.Output != addBase {
		t.Errorf("the verify command was handed poisoned content, not the commit's:\n%s", res.Output)
	}
}

// TestVerifyReportsBadCommitWithoutDestroyingTheTree is the other half of the
// same fix: a commit-ish git cannot resolve is the CALLER's fault, so it must
// not cost the repo its reuse tree.
func TestVerifyReportsBadCommitWithoutDestroyingTheTree(t *testing.T) {
	repoTop, base := newBaseRepo(t)
	reuse := reuseTreePath(t, repoTop)
	mustVerify(t, repoTop, base, "sh", "-c", "exit 0")
	head := gitT(t, reuse, "rev-parse", "HEAD")

	if _, err := Verify(context.Background(), repoTop, "no-such-ref", []string{cmdTrue}); err == nil {
		t.Fatalf("Verify accepted an unresolvable commit-ish")
	}

	if n := countRegistered(t, repoTop, reuse); n != 1 {
		t.Errorf("reuse tree registered %d times after a bad commit-ish, want 1", n)
	}
	if got := gitT(t, reuse, "rev-parse", "HEAD"); got != head {
		t.Errorf("a bad commit-ish moved the reuse tree from %s to %s", head, got)
	}
	if res := mustVerify(t, repoTop, base, "sh", "-c", "exit 0"); res.Tree != TreeReuse {
		t.Errorf("the verify after a bad commit-ish ran tree=%q, want %q", res.Tree, TreeReuse)
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
	if res.Tree != TreeFresh {
		t.Errorf("verify under a held lock ran tree=%q, want %q", res.Tree, TreeFresh)
	}
	if res.TreeReason == "" {
		t.Errorf("tree=fresh carries no reason; the caller cannot tell a lock from a broken repo")
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
