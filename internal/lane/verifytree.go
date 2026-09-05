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
// that must NOT trigger a re-cut, and it is now reserved for a commit-ish git
// cannot resolve AT ALL — see reuseVerifyTree for why the weaker reading of it
// was a bug.
var (
	errVerifyTreeUnusable = errors.New("lane: verify tree is not a live worktree")
	errVerifyTreeCheckout = errors.New("lane: verify tree refused a valid commit")
	errVerifyTreeCommit   = errors.New("lane: commit-ish does not resolve")
	errVerifyTreeDirty    = errors.New("lane: verify tree still dirty after reset")
	errNoGitCommonDir     = errors.New("lane: git-common-dir is empty")
)

// VerifyTreeMode names which checkout ran a verify. Reported on every
// VerifyResult so a caller can see the reuse tree working — or, more to the
// point, silently NOT working: a repo stuck on "fresh" is paying the cut this
// package exists to remove, and without this field that is invisible.
type VerifyTreeMode string

const (
	// TreeReuse: the standing per-repo worktree, reset in place. The fast path.
	TreeReuse VerifyTreeMode = "reuse"
	// TreeRecut: the standing worktree would not reset, so it was rebuilt.
	// Self-healing but slow — one cut, then reuse resumes.
	TreeRecut VerifyTreeMode = "recut"
	// TreeFresh: a throwaway under a temp dir. The reuse tree was unavailable
	// (no common dir, a peer holds the lock, or even the rebuild failed).
	TreeFresh VerifyTreeMode = "fresh"
)

// verifyTree is one acquired checkout: where the verify command runs, which
// mode delivered it, why that mode was not reuse, and how to give it back.
// release is always non-nil and safe to call once.
type verifyTree struct {
	path    string
	mode    VerifyTreeMode
	reason  string
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
//
// ‡ Every fallback carries its reason out to the caller. A silent fallback is
// indistinguishable from the feature working, so a repo permanently stuck on a
// throwaway cut would look exactly like a repo on the fast path while paying
// seconds per promotion — that is the failure mode this reporting exists for.
func acquireVerifyTree(ctx context.Context, g gitRunner, repoTop, commit string) (verifyTree, error) {
	common, err := gitCommonDir(ctx, g, repoTop)
	if err != nil {
		return freshVerifyTree(ctx, g, commit, oneLine(err.Error()))
	}
	lock, err := tryVerifyFlock(filepath.Join(common, verifyTreeLock))
	if err != nil {
		return freshVerifyTree(ctx, g, commit, "reuse tree held by a peer verify")
	}
	path := filepath.Join(common, verifyTreeDir, verifyTreeLeaf)
	mode, reason, err := resetVerifyTree(ctx, g, path, commit)
	if err != nil {
		lock.release()
		return freshVerifyTree(ctx, g, commit, oneLine(err.Error()))
	}
	return verifyTree{path: path, mode: mode, reason: reason, release: lock.release}, nil
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
// It returns the mode that delivered the tree and, when that is not TreeReuse,
// the one-line reason the reuse attempt was abandoned.
func resetVerifyTree(ctx context.Context, g gitRunner, path, commit string) (VerifyTreeMode, string, error) {
	err := reuseVerifyTree(ctx, g, path, commit)
	switch {
	case err == nil:
		return TreeReuse, "", nil
	case errors.Is(err, errVerifyTreeCommit):
		// Nothing to rebuild toward: git cannot resolve what the caller asked
		// for. Re-cutting would destroy a healthy tree over a bad argument.
		return "", "", err
	}
	reason := oneLine(err.Error())
	if err := recutVerifyTree(ctx, g, path, commit); err != nil {
		return "", "", err
	}
	return TreeRecut, reason, nil
}

// reuseVerifyTree resets the standing tree in place and reports what stopped
// it when it cannot.
//
// ‡ A failing checkout is the TREE's fault until the commit is proved missing.
// The first cut of this code read any checkout failure as a bad commit-ish and
// therefore never re-cut — and a tree can be poisoned into refusing EVERY
// commit forever: `git update-index --assume-unchanged <file>` plus an edit
// makes checkout bail on a dirty entry it has been told not to look at, and
// `clean` will not touch a tracked file. That left the reuse tree permanently
// dead with no signal, every promotion silently back to a full cut. So ask git
// whether the commit resolves before blaming it; if it does, the tree is at
// fault and gets rebuilt.
func reuseVerifyTree(ctx context.Context, g gitRunner, path, commit string) error {
	if !verifyTreeUsable(ctx, g, path) {
		return errVerifyTreeUnusable
	}
	if _, err := g.run(ctx, gitCall{args: []string{"-C", path, "clean", "-ffdx"}}); err != nil {
		return fmt.Errorf("lane: verify tree clean: %w", err)
	}
	if _, err := g.run(ctx, gitCall{args: []string{"-C", path, "checkout", gitForce, gitDetach, commit}}); err != nil {
		if commitResolves(ctx, g, commit) {
			return fmt.Errorf("%w: %w", errVerifyTreeCheckout, err)
		}
		return fmt.Errorf("%w: %w", errVerifyTreeCommit, err)
	}
	return verifyTreeIsClean(ctx, g, path)
}

// commitResolves reports whether git can name commit as a commit object in
// this repo. Asked from repoTop, which shares the object store with every
// linked worktree, so a poisoned worktree cannot skew the answer.
func commitResolves(ctx context.Context, g gitRunner, commit string) bool {
	_, err := g.run(ctx, gitCall{args: []string{"cat-file", "-e", commit + "^{commit}"}})
	return err == nil
}

// oneLine flattens a git error into something a single ℹ row can carry: git
// writes multi-line stderr, and a reason field that wraps breaks the one
// row-per-fact shape the CLI output contract asks for.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const limit = 160
	if len(s) > limit {
		return s[:limit-1] + "…"
	}
	return s
}

// verifyTreeIsClean is the runtime guard on the byte-identical rule: after a
// reset, the tree must look exactly like a fresh cut. Cheap next to the cut it
// replaces, and it turns "the reset quietly missed something" from a weakened
// verify into a re-cut.
//
// ‡ Two questions, because one does not cover the other. `status --porcelain`
// finds ordinary drift. It is BLIND, by design, to an entry flagged
// skip-worktree or assume-unchanged — those flags tell git to stop looking at
// a path, so an edit under one shows nothing in status, survives
// `checkout --force`, and would be handed to the verify command as if it were
// the commit's content. `git ls-files -v` tags every index entry and only 'H'
// means git is still watching, so it is the question status cannot answer.
func verifyTreeIsClean(ctx context.Context, g gitRunner, path string) error {
	out, err := g.run(ctx, gitCall{args: []string{"-C", path, "status", "--porcelain"}})
	if err != nil {
		return fmt.Errorf("lane: verify tree status: %w", err)
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("%w: %s", errVerifyTreeDirty, s)
	}
	tags, err := g.run(ctx, gitCall{args: []string{"-C", path, "ls-files", "-v"}})
	if err != nil {
		return fmt.Errorf("lane: verify tree index tags: %w", err)
	}
	for line := range strings.SplitSeq(tags, "\n") {
		if line != "" && line[0] != 'H' {
			return fmt.Errorf("%w: index entry not tracked normally: %s", errVerifyTreeDirty, line)
		}
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
func freshVerifyTree(ctx context.Context, g gitRunner, commit, reason string) (verifyTree, error) {
	parent, err := os.MkdirTemp("", "loto-verify-")
	if err != nil {
		return verifyTree{}, fmt.Errorf("lane: verify tempdir: %w", err)
	}
	wt := filepath.Join(parent, verifyTreeLeaf)
	if err := cutDetachedWorktree(ctx, g, wt, commit); err != nil {
		_ = os.RemoveAll(parent)
		return verifyTree{}, err
	}
	return verifyTree{path: wt, mode: TreeFresh, reason: reason, release: func() {
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
