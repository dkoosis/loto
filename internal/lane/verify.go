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
	"time"
)

// verifyTimeout caps the broad-repo command. Unlike gitTimeout (fast plumbing),
// the verify command is the slow path — `go test -race ./...` over a whole
// module — so the bound is minutes, not seconds. It only guards a wedged
// command; a shorter caller ctx deadline, if any, still wins.
const verifyTimeout = 15 * time.Minute

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
	// Output is the command's combined stdout+stderr with absolute
	// ephemeral-worktree and git-dir paths scrubbed to repo-relative form.
	Output string
}

// Verify runs a broad-repo command (go test -trimpath / vet / lint / build)
// against commit in a throwaway, detached worktree cut off the lane ref, then
// removes that worktree BY PATH. The command runs EXEC-ONLY: the caller never
// receives a writable handle to the checkout, so it can neither poison the
// prompt cache with the throwaway path nor silently lose edits into a tree about
// to be deleted. Both the worktree's checkout dir and its .git/.../worktrees/<id>
// admin path are stripped from Output — `go test -trimpath` removes them at the
// source for Go tooling; this scrub is the backstop for non-Go tools (vet
// plugins, linters, shell) that print absolute paths.
//
// commit is any commit-ish; in the lane pipeline it is the tip Commit returned.
// repoTop is the source working tree the worktree forks from (Commit threads the
// same RepoTop). A non-zero command exit is reported via VerifyResult.Passed,
// not as an error — a returned error means the verify could not be RUN (worktree
// setup/teardown failed, the command could not start, or ctx expired).
//
// Concurrency: each call removes only its own worktree by exact path and NEVER
// runs `git worktree prune`, so parallel lane verifies in one shared repo cannot
// reap each other's in-flight worktrees.
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

	// Parent dir for the throwaway checkout: `git worktree add` wants the leaf to
	// not yet exist, so point it at <tmp>/wt under a dir we own and later delete.
	parent, err := os.MkdirTemp("", "loto-verify-")
	if err != nil {
		return VerifyResult{}, fmt.Errorf("lane: verify tempdir: %w", err)
	}
	defer os.RemoveAll(parent)
	wt := filepath.Join(parent, "wt")

	if _, err := g.run(ctx, gitCall{args: []string{"worktree", "add", "--detach", wt, commit}}); err != nil {
		return VerifyResult{}, fmt.Errorf("lane: worktree add: %w", err)
	}
	// Tear down BY PATH (never prune — prune would reap peers' concurrent
	// worktrees). Background ctx so cleanup still runs if the caller's ctx expired.
	defer func() {
		_, _ = g.run(context.Background(), gitCall{args: []string{"worktree", "remove", wt, "--force"}})
	}()

	// Learn the worktree's git admin dir so its absolute form can be scrubbed too;
	// a non-fatal best effort (a tool that never prints it costs us nothing).
	gitDir, _ := g.run(ctx, gitCall{args: []string{"-C", wt, gitRevParse, "--absolute-git-dir"}})

	out, passed, err := runVerifyCmd(ctx, wt, cmd)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Passed: passed, Output: scrubPaths(out, wt, strings.TrimSpace(gitDir))}, nil
}

// runVerifyCmd runs cmd with cwd=dir, capturing combined stdout+stderr. A
// non-zero exit returns (output, false, nil) — a verify result, not an infra
// failure. A start error or ctx-expiry returns a non-nil error. The command
// inherits the parent environment so it finds the global, content-addressed Go
// caches (GOCACHE/GOMODCACHE) — a fresh worktree reuses them.
func runVerifyCmd(ctx context.Context, dir string, cmd []string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	//nolint:gosec // G204: cmd is the caller-supplied verify command (go test / vet / lint / build), run exec-only in a detached worktree; never shell-interpreted here.
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = dir
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
		return buf.String(), false, fmt.Errorf("%w: %v", errVerifyAborted, ctx.Err())
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
