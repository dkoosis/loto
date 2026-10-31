package cli

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnlockAll_FallbackWhenNoSessionPin covers the no-LOTO_SESSION_ID path:
// without pinning, --all stays agent-scoped — the safe fallback for direct
// CLI use where each invocation would otherwise mint a different session id
// and --all would match nothing.
func TestUnlockAll_FallbackWhenNoSessionPin(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_SESSION_ID", "") // explicitly unset; each Run() mints a fresh id

	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "lock1"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock a.go: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, "lock2"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock store.go: exit %d", code)
	}

	var out bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "cleanup"}, &out, io.Discard); code != 0 {
		t.Fatalf("unlock --all: exit %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "count=2") {
		t.Errorf("no-pin --all should release both agent-owned locks; got: %s", out.String())
	}
}

// siblingCheckouts builds the loto-19bz shape: one repo (withTempProject's,
// on branch fixer-a with one commit) plus a linked worktree of it on fixer-b.
// Both checkouts share one origin, so one store; the owner id is pinned and
// LOTO_SESSION_ID is shared — owner and session scoping collapse onto one
// identity, exactly the bare-CLI case of two sibling subagents inheriting one
// parent Claude Code session's env. Returns (primary, linked); cwd is primary.
func siblingCheckouts(t *testing.T) (string, string) {
	t.Helper()
	repo := withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_SESSION_ID", "shared-session")
	runGitIn(t, repo, "checkout", "-q", "-b", "fixer-a")
	runGitIn(t, repo, "commit", "--allow-empty", "-q", "-m", "init")
	wt := filepath.Join(t.TempDir(), "fixer-b")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "fixer-b", wt)
	return repo, wt
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestUnlockAll_BranchSwitchInOwnCheckoutReleases is Codex #371 P1 "use
// checkout identity instead of branch names": a session that locks, switches
// branch in the SAME worktree, then runs `unlock --all` (the SessionEnd hook's
// call) is releasing its own lock. A branch label is not a checkout, so this
// must sweep, not refuse and leave the lock squatting until TTL.
func TestUnlockAll_BranchSwitchInOwnCheckoutReleases(t *testing.T) {
	repo, _ := siblingCheckouts(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock a.go: exit %d", code)
	}
	runGitIn(t, repo, "checkout", "-q", "-b", "fixer-a-2")

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "session end"}, &out, &errBuf); code != 0 {
		t.Fatalf("--all after a branch switch in the same checkout must release, got exit %d: stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "count=1") {
		t.Errorf("want the one lock released; got: %s", out.String())
	}
}
