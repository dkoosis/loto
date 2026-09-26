package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnlockAll_AfterWorktreeMove_ReleasesOwnRowsAndKeepsScoping is the
// loto-in0v regression: `git worktree move` (or a bare rename of the
// checkout directory) changes rt.RepoTop while LockRecord.Worktree /
// ClaimRecord.Worktree keep the path they were stamped with, so from the new
// path this checkout's own rows read as another worktree's — unlock --all
// refuses (fail-safe ambiguity) and conflict scoping stops seeing a peer's
// lock in the SAME, now-moved worktree as a same-worktree conflict at all.
//
// alice takes a lock + a claim in a linked worktree, which is then moved.
// From the new path: bob (a different owner, same worktree) must still see
// alice's exclusive lock as a live, same-worktree conflict (AC2) — and must
// NOT see it as a conflict from a genuinely different, untouched worktree
// (AC3). Finally alice's own unlock --all from the new path must release
// both the lock and the claim with no ambiguity refusal (AC1).
func TestUnlockAll_AfterWorktreeMove_ReleasesOwnRowsAndKeepsScoping(t *testing.T) {
	repo := withTempProject(t)
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "seed")

	linkedOld := filepath.Join(t.TempDir(), "agent-moved")
	gitT(t, repo, "worktree", "add", "-q", "--detach", linkedOld, tcHEAD)
	other := filepath.Join(t.TempDir(), "agent-other")
	gitT(t, repo, "worktree", "add", "-q", "--detach", other, tcHEAD)

	alice, bob := twoAgents(t)
	asAlice := func() { t.Setenv("LOTO_AGENT_ID", alice.UUID) }
	asBob := func() { t.Setenv("LOTO_AGENT_ID", bob.UUID) }
	run := func(argv ...string) (string, string, int) {
		var out, errBuf bytes.Buffer
		code := Run(argv, &out, &errBuf)
		return out.String(), errBuf.String(), code
	}

	// alice takes a lock and a claim in the worktree that is about to move.
	t.Chdir(linkedOld)
	asAlice()
	if _, errBuf, code := run(tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest); code != 0 {
		t.Fatalf("seed lock: exit=%d stderr=%q", code, errBuf)
	}
	if _, errBuf, code := run(tcCmdClaim, tcPrefixStore, tcFlagIntent, tcIntentTest); code != 0 {
		t.Fatalf("seed claim: exit=%d stderr=%q", code, errBuf)
	}

	linkedNew := filepath.Join(filepath.Dir(linkedOld), "agent-moved-renamed")
	gitT(t, repo, "worktree", "move", linkedOld, linkedNew)

	// AC3: a genuinely different worktree still reads alice's lock as
	// foreign — no conflict at all (not even advisory).
	t.Chdir(other)
	asBob()
	if out, errBuf, code := run(tcCmdCheck, tcTargetA); code != 0 || !strings.Contains(out, "✓ no conflicts") {
		t.Fatalf("check from an unrelated worktree must see no conflict, got exit=%d out=%q stderr=%q", code, out, errBuf)
	}

	// AC2: from the NEW path, the SAME worktree's lock still surfaces as a
	// conflict candidate for a different owner — conflict scoping
	// (domain.SameWorktree) did not lose track of it across the move. Whether
	// it hard-blocks is a separate, unrelated liveness question (loto-k5el.2);
	// what loto-in0v owns is that the row is seen as belonging to this
	// worktree at all.
	t.Chdir(linkedNew)
	if out, _, _ := run(tcCmdCheck, tcTargetA); !strings.Contains(out, "conflicts count=1") {
		t.Fatalf("check from the moved worktree must still see alice's lock as a same-worktree conflict, got out=%q", out)
	}

	// AC1: alice's own unlock --all from the new path releases both rows,
	// with no ambiguity refusal.
	asAlice()
	out, errBuf, code := run(tcCmdUnlock, tcFlagAll, tcFlagIntent, tcIntentDone)
	if code != 0 {
		t.Fatalf("unlock --all after move: exit=%d out=%q stderr=%q", code, out, errBuf)
	}
	if strings.Contains(errBuf, "identity ambiguous") {
		t.Fatalf("unlock --all after move must not refuse as ambiguous: stderr=%q", errBuf)
	}
	if !strings.Contains(out, "target="+tcTargetA) {
		t.Errorf("unlock --all after move should report the lock released: out=%q", out)
	}
	if !strings.Contains(out, "claims-released count=1") {
		t.Errorf("unlock --all after move should report the claim released: out=%q", out)
	}
}
