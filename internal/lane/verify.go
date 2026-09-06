package lane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// verifyTimeout caps the broad-repo command. Unlike gitTimeout (fast plumbing),
// the verify command is the slow path — `go test -race ./...` over a whole
// module — so the bound is minutes, not seconds. It only guards a wedged
// command; a shorter caller ctx deadline, if any, still wins.
const verifyTimeout = 15 * time.Minute

// verifyWaitDelay bounds how long c.Wait keeps draining the command's
// stdout/stderr pipe after Cancel fires. Left at zero (the default), Wait
// reads the pipe until EOF, which does not occur until every process holding
// the write end closes it — including an orphaned grandchild that survives
// the direct child's death (loto-wtxe). A few seconds is enough for the
// process group's fds to close once SIGKILLed; it is not a second verify
// budget.
const verifyWaitDelay = 5 * time.Second

// errVerifyInput is the stable target for a malformed Verify call (repo
// convention: static error, not an ad-hoc string).
var errVerifyInput = errors.New("lane: invalid Verify input")

// errVerifyAborted is the stable target for a verify that could not RUN to a
// verdict because the ctx expired (caller deadline/cancel, or the internal
// verifyTimeout wedge guard). Distinct from errVerifyInput on purpose: a ctx
// abort is an infrastructure timeout, not bad input and not a failing test, so
// lane choreography must remediate it as infra (retry/escalate), never as
// "your tests fail". A ctx-killed command surfaces from exec as an
// *exec.ExitError ("signal: killed"); this sentinel reclassifies that case.
var errVerifyAborted = errors.New("lane: verify aborted before a verdict")

// VerifyResult is the outcome of one hermetic verify run.
type VerifyResult struct {
	// Passed is true iff the command exited zero.
	Passed bool
	// Output is the command's combined stdout+stderr with the absolute
	// verify-worktree and git-dir paths scrubbed to repo-relative form.
	Output string
	// Tree names which checkout ran the command: reuse (the fast path), recut
	// (the standing tree was rebuilt first), or fresh (a throwaway).
	Tree VerifyTreeMode
	// TreeReason is one line saying why Tree is not reuse; empty when it is.
	// Set even on the RECUT path, which is self-healing and would otherwise
	// leave no trace of what poisoned the tree.
	TreeReason string
}

// Verify runs a broad-repo command (go test -trimpath / vet / lint / build)
// against commit in a detached worktree holding exactly that commit — never
// the shared, dirty checkout. The command runs EXEC-ONLY: the caller never
// receives a writable handle to the checkout, so it can neither poison the
// prompt cache with the worktree path nor silently lose edits into a tree that
// the next verify resets. Both the worktree's checkout dir and its
// .git/.../worktrees/<id> admin path are stripped from Output — `go test
// -trimpath` removes them at the source for Go tooling; this scrub is the
// backstop for non-Go tools (vet plugins, linters, shell) that print absolute
// paths.
//
// commit is any commit-ish; in the lane pipeline it is the tip Commit returned.
// repoTop is the source working tree the worktree forks from (Commit threads the
// same RepoTop). A non-zero command exit is reported via VerifyResult.Passed,
// not as an error — a returned error means the verify could not be RUN (worktree
// setup/teardown failed, the command could not start, or ctx expired).
//
// ‡ The worktree is REUSED, one per repo, under the git common dir — cutting a
// fresh one cost 2.9–7.1s on this repo's 327 files and was the whole p50 of a
// promotion's phase-2 verify (loto-3is6). It is reset to commit before the
// command runs, so what the command sees is byte-identical to a fresh cut;
// verifytree.go carries the reset and the fallbacks.
//
// Concurrency: the reused tree is held under its own non-blocking flock, so a
// second verify in the same repo cuts a throwaway rather than waiting or
// sharing. Every teardown removes ONLY its own worktree by exact path and
// NEVER runs `git worktree prune`, so parallel lane verifies in one shared
// repo cannot reap each other's in-flight worktrees.
func Verify(ctx context.Context, repoTop, commit string, cmd []string) (VerifyResult, error) {
	switch {
	case repoTop == "":
		return VerifyResult{}, fmt.Errorf("%w: repoTop", errVerifyInput)
	case commit == "":
		return VerifyResult{}, fmt.Errorf("%w: commit", errVerifyInput)
	case len(cmd) == 0 || cmd[0] == "":
		return VerifyResult{}, fmt.Errorf("%w: cmd", errVerifyInput)
	}

	g := gitRunner{repoTop: repoTop}

	// The checkout: the repo's reused verify worktree when it can be had, a
	// throwaway cut when it cannot. Either way the tree is detached at commit
	// and clean, and release either drops the reuse lock or removes the
	// throwaway BY PATH. verifytree.go holds which case is chosen and why.
	tree, err := acquireVerifyTree(ctx, g, repoTop, commit)
	if err != nil {
		return VerifyResult{}, err
	}
	defer tree.release()
	wt := tree.path

	// Learn the worktree's git admin dir so its absolute form can be scrubbed too;
	// a non-fatal best effort (a tool that never prints it costs us nothing).
	gitDir, _ := g.run(ctx, gitCall{args: []string{"-C", wt, gitRevParse, "--absolute-git-dir"}})

	out, passed, err := runVerifyCmd(ctx, wt, cmd)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Passed:     passed,
		Output:     scrubPaths(out, wt, strings.TrimSpace(gitDir)),
		Tree:       tree.mode,
		TreeReason: tree.reason,
	}, nil
}

// runVerifyCmd runs cmd with cwd=dir, capturing combined stdout+stderr. A
// non-zero exit returns (output, false, nil) — a verify result, not an infra
// failure. A start error or ctx-expiry returns a non-nil error. The command
// inherits the parent environment so it finds the global, content-addressed Go
// caches (GOCACHE/GOMODCACHE) — a fresh worktree reuses them. GOWORK is
// overridden to "off" (codex #286 finding 4): a caller with GOWORK exported to
// an absolute path in the source checkout would make cmd resolve modules
// against that workspace file instead of the throwaway worktree's own go.mod
// — quietly voiding the "this is the commit's tree, isolated" guarantee
// Verify sells. No go.work exists in this repo today, so this guards a real
// but not-yet-triggered failure mode, not one observed here.
func runVerifyCmd(ctx context.Context, dir string, cmd []string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	//nolint:gosec // G204: cmd is the caller-supplied verify command (go test / vet / lint / build), run exec-only in a detached worktree; never shell-interpreted here.
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = dir
	// append, not assign: os.Environ() first so a later, duplicate key wins
	// (Go's exec passes duplicate env keys through to the OS, which applies
	// the LAST occurrence) — this overrides two entries the caller's shell may
	// have exported, not a fresh environment.
	//
	// PWD=dir is required alongside GOWORK, not cosmetic: when Env is left nil,
	// Go's exec auto-injects a PWD matching Dir for us; the moment ANY Env is
	// set explicitly, that auto-injection stops, and the child inherits our
	// PARENT's (stale, unrelated-directory) PWD instead. A shell's `pwd`
	// builtin trusts a stale-but-`stat`-valid PWD over the real cwd — sh finds
	// the inherited PWD doesn't match dir, falls back to a raw getcwd(), and
	// prints the SYMLINK-RESOLVED form (macOS: /var -> /private/var) rather
	// than the literal dir string scrubPaths knows how to strip, leaking an
	// absolute path into --build output. Setting PWD=dir ourselves is what
	// nil-Env was already doing implicitly; this line preserves that, not a
	// new behavior.
	c.Env = append(os.Environ(), "GOWORK=off", "PWD="+dir)
	// Run in its own process group: make/go test spawn grandchildren, and
	// without this, killing only the direct child (exec's default Cancel)
	// leaves them holding the stdout/stderr pipe open, so Wait blocks on
	// draining it long after ctx expired (loto-wtxe). Setpgid with no Pgid
	// makes the child the group leader, so its own pid doubles as the group id.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Override the default Cancel (Process.Kill, direct child only) to SIGKILL
	// the whole group, so an orphaned grandchild dies with its parent instead
	// of surviving ctx expiry.
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = verifyWaitDelay
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf

	err := c.Run()
	if err == nil {
		return buf.String(), true, nil
	}
	// Check ctx expiry BEFORE classifying *exec.ExitError as a RED verdict. A ctx
	// deadline/cancel (caller's, or the verifyTimeout wedge guard) kills the running
	// command, and exec reports that kill as an *exec.ExitError ("signal: killed").
	// Treating it as Passed=false would mislabel an infra timeout as failing tests;
	// surface it as an infra error so the lane retries/escalates instead.
	if ctx.Err() != nil {
		return buf.String(), false, fmt.Errorf("%w: %w", errVerifyAborted, ctx.Err())
	}
	exitErr := new(exec.ExitError)
	if errors.As(err, &exitErr) {
		return buf.String(), false, nil
	}
	return buf.String(), false, fmt.Errorf("lane: verify command %q: %w", cmd[0], err)
}

// scrubPaths rewrites the absolute worktree/git-dir paths in out to repo-relative
// form. Each path is scrubbed in both its as-created and symlink-resolved form,
// because a child's getcwd() canonicalizes (macOS /var -> /private/var) so the
// path a tool prints need not match the one we handed to git.
func scrubPaths(out string, paths ...string) string {
	seen := map[string]bool{}
	for _, p := range paths {
		for _, v := range pathVariants(p) {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = strings.ReplaceAll(out, v+string(os.PathSeparator), "")
			out = strings.ReplaceAll(out, v, ".")
		}
	}
	return out
}

// pathVariants returns p plus, when it differs, its symlink-resolved form.
func pathVariants(p string) []string {
	if p == "" {
		return nil
	}
	vs := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		vs = append(vs, r)
	}
	return vs
}
