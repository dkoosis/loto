package cli

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnlockAll_ScopedToSession exercises the loto-81n fix: SessionEnd
// release (unlock --all) must not drop locks held by sibling sessions of
// the same agent. Two LOTO_SESSION_IDs share one LOTO_AGENT_ID; --all in
// session-1 must leave session-2's lock intact.
func TestUnlockAll_ScopedToSession(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // sets LOTO_AGENT_ID to a single agent across all Run() calls below

	// Session 1 locks a.go.
	t.Setenv("LOTO_SESSION_ID", "session-one")
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "s1 work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session-1 lock a.go: exit %d", code)
	}

	// Session 2 locks internal/store/store.go (same agent, different session).
	t.Setenv("LOTO_SESSION_ID", "session-two")
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, "s2 work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session-2 lock store.go: exit %d", code)
	}

	// Back to session-1: --all should release only session-1's holdings.
	t.Setenv("LOTO_SESSION_ID", "session-one")
	var out bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "session-1 end"}, &out, io.Discard); code != 0 {
		t.Fatalf("session-1 unlock --all: exit %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "count=1") {
		t.Errorf("session-1 should release exactly 1 lock; got: %s", out.String())
	}

	// Session-2's lock must survive.
	t.Setenv("LOTO_SESSION_ID", "session-two")
	out.Reset()
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, io.Discard); code != 0 {
		t.Fatalf("status --mine: exit %d", code)
	}
	if !strings.Contains(out.String(), "store.go") {
		t.Errorf("session-2's store.go lock should survive session-1's --all; got: %s", out.String())
	}
	if strings.Contains(out.String(), "a.go") {
		t.Errorf("a.go should have been released by session-1's --all; got: %s", out.String())
	}
}

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

// TestUnlockAll_AmbiguousIdentityAcrossWorktreesRefuses is loto-19bz's own AC:
// "session A" locks a.go in one checkout; "session B" — same owner, same
// session, nothing of its own locked, standing in a sibling worktree — runs
// `unlock --all`. It must refuse (non-zero, naming the other checkout's lock)
// rather than release a.go, and `loto status` must still show it.
func TestUnlockAll_AmbiguousIdentityAcrossWorktreesRefuses(t *testing.T) {
	_, wt := siblingCheckouts(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "fixer-a work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session A lock a.go: exit %d", code)
	}

	t.Chdir(wt)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "fixer-b cleanup"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("session B's --all should refuse, got exit 0: out=%s", out.String())
	}
	if !strings.Contains(errBuf.String(), "ambiguous") || !strings.Contains(errBuf.String(), tcTargetA) {
		t.Errorf("refusal should name the other checkout's a.go; stderr=%q", errBuf.String())
	}

	out.Reset()
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, io.Discard); code != 0 {
		t.Fatalf("status --mine: exit %d", code)
	}
	if !strings.Contains(out.String(), tcTargetA) {
		t.Errorf("a.go should still be locked after the refused sweep; got: %s", out.String())
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

// TestUnlockAll_ClaimOnlySiblingInOtherCheckoutRefuses is Codex #371 P1
// "protect claim-only sibling state": sibling A holds only a territory claim,
// no lock. Sibling B, in another checkout under the collapsed identity, runs
// `unlock --all`. The sweep must refuse and leave A's claim standing — a
// lock-rows-only ambiguity check sees nothing and commits the claim delete.
func TestUnlockAll_ClaimOnlySiblingInOtherCheckoutRefuses(t *testing.T) {
	_, wt := siblingCheckouts(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", "fixer-a territory"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session A claim: exit %d", code)
	}

	t.Chdir(wt)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "fixer-b cleanup"}, &out, &errBuf); code == 0 {
		t.Fatalf("session B's --all should refuse over A's claim, got exit 0: out=%s", out.String())
	}
	if !strings.Contains(errBuf.String(), tcPrefixStore) {
		t.Errorf("refusal should name A's claimed prefix; stderr=%q", errBuf.String())
	}

	out.Reset()
	if code := Run([]string{tcCmdStatus}, &out, io.Discard); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(out.String(), tcPrefixStore) {
		t.Errorf("A's claim should survive B's refused sweep; got: %s", out.String())
	}
}
