package lane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// verifytree.go holds the checkout Verify runs in: one reused, detached
// worktree per repo, plus the flock that keeps two verifies out of it and the
// throwaway cut that covers every case the reuse cannot serve.
//
// ‡ Why reuse at all. `git worktree add --detach` on this repo's 327-file tree
// measured 2.9–7.1s (loto-3is6, 2026-09-05) — the whole p50 of a promotion's
// phase-2 verify, with the invariant command itself contributing nothing. The
// cut is the cost, so the fix is to stop paying it per verify: check the chain
// tip out into a worktree that already exists, and leave it standing.
//
// ‡ Reuse must not weaken the verify. A reset tree is byte-identical to a
// fresh cut — `git clean -fdx` then `git checkout --force --detach <commit>`
// leaves the same index and an empty `git status --porcelain`. Anything the
// reset cannot guarantee (missing tree, stale registration, a git error)
// re-cuts in place; anything that cannot be re-cut falls back to a throwaway.

const (
	// verifyTreeDir is the leaf under --git-common-dir that holds the reused
	// checkout. Under .git, never in the shared checkout: the checkout is
	// where agents work, and a 327-file sibling tree parked in it would show
	// up in every `ls`, every editor tree and every `go build ./...` walk.
	verifyTreeDir = "loto-verify"
	// verifyTreeLeaf is the checkout itself. `git worktree add` wants a leaf
	// that does not yet exist, so the dir above it is ours to create.
	verifyTreeLeaf = "wt"
	// verifyTreeLock guards the reused tree. Beside it, under the common dir,
	// so two linked worktrees of one repo — which share one .git and therefore
	// one reuse tree — contend on one file.
	verifyTreeLock = "loto-verify.lock"
)

// git subcommand tokens, spelled once (repo convention, φ gitRevParse).
const (
	gitWorktree = "worktree"
	gitDetach   = "--detach"
	gitForce    = "--force"
)

// Reset outcomes the caller discriminates on. errVerifyTreeCommit is the one
// that must NOT trigger a re-cut: the tree answered git and cleaned fine, so
// the fault is the caller's commit-ish and destroying a healthy tree over it
// would trade a bad argument for a slow next verify.
var (
	errVerifyTreeUnusable = errors.New("lane: verify tree is not a live worktree")
	errVerifyTreeCommit   = errors.New("lane: verify tree cannot check out the commit")
	errVerifyTreeDirty    = errors.New("lane: verify tree still dirty after reset")
	errNoGitCommonDir     = errors.New("lane: git-common-dir is empty")
)

// verifyTree is one acquired checkout: where the verify command runs, and how
// to give it back. release is always non-nil and safe to call once.
type verifyTree struct {
	path    string
	release func()
}

// acquireVerifyTree returns a detached checkout of commit for the verify to run
// in. It prefers the repo's reused tree and falls back to a throwaway cut on
// every path that cannot deliver one: no resolvable common dir, the lock held
// by a concurrent verify, or a tree that will neither reset nor re-cut.
//
// ‡ The fallback is a fallback, not a failure. A busy lock is the expected
// second-verify case (gate.Promote runs phase 2 with NO flock held, so two
// `loto promote` processes in one repo can verify at the same time, as can any
// mix of `loto verify` / `loto lane --build`); the loser cuts its own tree
// rather than waiting, because waiting would cost more than the cut it avoids.
func acquireVerifyTree(ctx context.Context, g gitRunner, repoTop, commit string) (verifyTree, error) {
	if common, err := gitCommonDir(ctx, g, repoTop); err == nil {
		if lock, err := tryVerifyFlock(filepath.Join(common, verifyTreeLock)); err == nil {
			path := filepath.Join(common, verifyTreeDir, verifyTreeLeaf)
			if err := resetVerifyTree(ctx, g, path, commit); err == nil {
				return verifyTree{path: path, release: lock.release}, nil
			}
			lock.release()
		}
	}
	return freshVerifyTree(ctx, g, commit)
}

// resetVerifyTree points the reused worktree at commit, leaving a checkout
// byte-identical to a fresh `git worktree add --detach`. A tree that will not
// reset is rebuilt from nothing.
//
// Order is load-bearing: clean FIRST, because a verify killed mid-run leaves
// build output and stray dirs behind, and an untracked dir sitting where the
// target commit wants a file makes the checkout fail; checkout --force SECOND,
// because clean does not touch tracked files a command edited in place.
//
// ‡ `-ffdx`, not `-fdx`. A single -f leaves an untracked directory that
// contains a .git behind — a nested repo a test created and abandoned — and a
// fresh cut has no such thing. The tree is loto's disposable scratch, so the
// closer-to-fresh reading of "force" is the right one.
func resetVerifyTree(ctx context.Context, g gitRunner, path, commit string) error {
	// Anything but a bad commit-ish is the TREE's problem, and a tree that
	// will not come back clean is replaced rather than handed to the command:
	// reuse must never weaken the verify.
	if err := reuseVerifyTree(ctx, g, path, commit); err == nil || errors.Is(err, errVerifyTreeCommit) {
		return err
	}
	return recutVerifyTree(ctx, g, path, commit)
}

// reuseVerifyTree resets the standing tree in place and reports what stopped
// it when it cannot.
func reuseVerifyTree(ctx context.Context, g gitRunner, path, commit string) error {
	if !verifyTreeUsable(ctx, g, path) {
		return errVerifyTreeUnusable
	}
	if _, err := g.run(ctx, gitCall{args: []string{"-C", path, "clean", "-ffdx"}}); err != nil {
		return fmt.Errorf("lane: verify tree clean: %w", err)
	}
	if _, err := g.run(ctx, gitCall{args: []string{"-C", path, "checkout", gitForce, gitDetach, commit}}); err != nil {
		return fmt.Errorf("%w: %w", errVerifyTreeCommit, err)
	}
	return verifyTreeIsClean(ctx, g, path)
}

// verifyTreeIsClean is the runtime guard on the byte-identical rule: after a
// reset, `git status --porcelain` must be as silent as it is in a fresh cut.
// Cheap next to the cut it replaces, and it turns "the reset quietly missed
// something" from a weakened verify into a re-cut.
func verifyTreeIsClean(ctx context.Context, g gitRunner, path string) error {
	out, err := g.run(ctx, gitCall{args: []string{"-C", path, "status", "--porcelain"}})
	if err != nil {
		return fmt.Errorf("lane: verify tree status: %w", err)
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("%w: %s", errVerifyTreeDirty, s)
	}
	return nil
}

// recutVerifyTree rebuilds the reused worktree from nothing. The removal is BY
// EXACT PATH and never `git worktree prune`: prune would reap a peer's
// in-flight verify worktree, which is the invariant Verify has always held.
// A registration whose checkout dir is gone is cleared by the same call.
func recutVerifyTree(ctx context.Context, g gitRunner, path, commit string) error {
	_, _ = g.run(ctx, gitCall{args: removeWorktreeArgs(path)})
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("lane: clear verify tree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("lane: verify tree dir: %w", err)
	}
	return cutDetachedWorktree(ctx, g, path, commit)
}

// cutDetachedWorktree is the one spelling of the cut, so the reuse tree and the
// throwaway cannot drift apart in what they hand the verify command.
func cutDetachedWorktree(ctx context.Context, g gitRunner, path, commit string) error {
	if _, err := g.run(ctx, gitCall{args: []string{gitWorktree, "add", gitDetach, path, commit}}); err != nil {
		return fmt.Errorf("lane: worktree add: %w", err)
	}
	return nil
}

// removeWorktreeArgs drops one worktree BY EXACT PATH. Never `git worktree
// prune`: prune reaps every stale registration in the repo, a peer's in-flight
// verify worktree included.
func removeWorktreeArgs(path string) []string {
	return []string{gitWorktree, "remove", gitForce, path}
}

// verifyTreeUsable reports whether path is a live linked worktree of this repo
// — registered with git AND still on disk. Registration is read from
// `git worktree list`, not guessed from a .git file, because git records the
// canonicalized path and only git knows what it wrote.
func verifyTreeUsable(ctx context.Context, g gitRunner, path string) bool {
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return false
	}
	out, err := g.run(ctx, gitCall{args: []string{gitWorktree, "list", "--porcelain"}})
	if err != nil {
		return false
	}
	want := canonPath(path)
	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), gitWorktree+" ")
		if ok && canonPath(rest) == want {
			return true
		}
	}
	return false
}

// canonPath is path with symlinks resolved when they resolve. macOS puts
// /var -> /private/var between the path we hand git and the one it stores, so
// comparing the raw strings would read a healthy tree as unregistered.
func canonPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// freshVerifyTree cuts a throwaway detached worktree under a temp dir we own,
// and tears it down BY PATH on release. This was Verify's only mode before the
// reuse tree; it is now the fallback, unchanged in behavior.
func freshVerifyTree(ctx context.Context, g gitRunner, commit string) (verifyTree, error) {
	parent, err := os.MkdirTemp("", "loto-verify-")
	if err != nil {
		return verifyTree{}, fmt.Errorf("lane: verify tempdir: %w", err)
	}
	wt := filepath.Join(parent, verifyTreeLeaf)
	if err := cutDetachedWorktree(ctx, g, wt, commit); err != nil {
		_ = os.RemoveAll(parent)
		return verifyTree{}, err
	}
	return verifyTree{path: wt, release: func() {
		// Background ctx so cleanup still runs when the caller's ctx expired.
		_, _ = g.run(context.Background(), gitCall{args: removeWorktreeArgs(wt)})
		_ = os.RemoveAll(parent)
	}}, nil
}

// gitCommonDir is the repo's shared .git, absolute. `--git-common-dir` rather
// than `--git-dir` on purpose: a linked worktree's own git dir is private to
// it, and the reuse tree is per REPO.
func gitCommonDir(ctx context.Context, g gitRunner, repoTop string) (string, error) {
	out, err := g.run(ctx, gitCall{args: []string{gitRevParse, "--git-common-dir"}})
	if err != nil {
		return "", fmt.Errorf("lane: resolve git-common-dir: %w", err)
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return "", errNoGitCommonDir
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoTop, dir)
	}
	return dir, nil
}

// verifyFlock is a held reuse-tree lock.
type verifyFlock struct{ f *os.File }

func (h *verifyFlock) release() {
	if h == nil || h.f == nil {
		return
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
	h.f = nil
}

// tryVerifyFlock takes the reuse-tree lock without waiting.
//
// ‡ Non-blocking on purpose, and a busy holder is not an error worth
// reporting: the whole point of the reuse tree is to be faster than a cut, so
// a verify that queued behind another one would lose the saving it came for.
// The caller reads any error as "cut your own tree". The kernel drops a flock
// when its holder dies, so a killed verify never wedges the next one.
func tryVerifyFlock(path string) (*verifyFlock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("lane: verify lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lane: open verify lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lane: flock %s: %w", path, err)
	}
	return &verifyFlock{f: f}, nil
}
