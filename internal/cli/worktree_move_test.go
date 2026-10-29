package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeMove_KeepsHoldLiveAndOwned is loto-hu8o's AC: a hold's
// worktree is the checkout's admin dir (worktrees/<name>), which `git
// worktree move` keeps, so a lock and a claim taken before the move are
// still live and still this checkout's after it, with no repair step. Live:
// a peer's lock on the held file, from the moved checkout, is refused. Owned:
// the holder's `unlock --all` from the new path releases both, with no
// ambiguity refusal.
func TestWorktreeMove_KeepsHoldLiveAndOwned(t *testing.T) {
	repo := withTempProject(t)
	holder := pinAgent(t)
	holderSession := os.Getenv("CLAUDE_CODE_SESSION_ID")
	runGitIn(t, repo, "checkout", "-q", "-b", "fixer-a")
	runGitIn(t, repo, "commit", "--allow-empty", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "fixer-b")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "fixer-b", wt)
	if err := os.WriteFile(filepath.Join(wt, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	runOK(t, tcCmdLock, tcTargetB, tcFlagIntent, "loto-hu8o: work")
	runOK(t, tcCmdClaim, tcPrefixStore, "-t", "loto-hu8o: territory")

	moved := filepath.Join(filepath.Dir(wt), "fixer-b-moved")
	runGitIn(t, repo, "worktree", "move", wt, moved)
	t.Chdir(moved)

	pinAgent(t) // a peer, in the moved checkout
	var peerOut, peerErr bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetB, tcFlagIntent, "peer"}, &peerOut, &peerErr); code == 0 {
		t.Fatalf("a peer's lock over the moved checkout's live hold must be refused: out=%q err=%q", peerOut.String(), peerErr.String())
	}

	t.Setenv("LOTO_AGENT_ID", holder.UUID)
	t.Setenv("CLAUDE_CODE_SESSION_ID", holderSession)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "loto-hu8o: done"}, &out, &errBuf); code != 0 {
		t.Fatalf("unlock --all from the moved checkout must release its own holds, exit %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if strings.Contains(errBuf.String(), "ambiguous") {
		t.Fatalf("the moved checkout's holds must read as its own: %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "count=1") || !strings.Contains(out.String(), tcPrefixStore) {
		t.Errorf("want the lock and the claim released: %q", out.String())
	}
	if code := Run([]string{tcCmdLock, tcTargetB, tcFlagIntent, "after"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("b.go must be free after the release")
	}
}
