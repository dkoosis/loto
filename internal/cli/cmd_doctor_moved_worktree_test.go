package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realpath resolves p's symlinks. On macOS, t.TempDir() paths live under a
// /var that is itself a symlink to /private/var, and git (and loto's own
// repo-top resolution) both resolve it fully — so a raw t.TempDir()-derived
// string can differ from the path that actually gets stamped on a lock or
// claim row even though both name the same directory.
func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", p, err)
	}
	return r
}

// TestDoctorMovedWorktree_ReportRepairThenUnlockAllReleases is loto-in0v's
// core AC: `git worktree move` changes rt.RepoTop while the lock and claim
// rows taken from the old path keep it, so this checkout's own rows read as
// a sibling's until repaired (Codex #371 comment 4111949212). `loto doctor`
// from the new path reports the stale stamps; `loto doctor --repair`
// rewrites them (the explicit fix dk chose over an automatic
// runtime-open migration — Rules); `unlock --all` then releases both with
// no ambiguity refusal.
func TestDoctorMovedWorktree_ReportRepairThenUnlockAllReleases(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	runGitIn(t, repo, "checkout", "-q", "-b", "fixer-a")
	runGitIn(t, repo, "commit", "--allow-empty", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), "fixer-b")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "fixer-b", wt)
	// git resolves symlinks in the path it records for the worktree (macOS's
	// /var -> /private/var), so the stamp loto records will carry the
	// resolved form even though wt itself does not yet — resolve here so the
	// string this test compares against matches what actually gets stamped.
	wt = realpath(t, wt)

	if err := os.WriteFile(filepath.Join(wt, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(wt)
	if code := Run([]string{tcCmdLock, tcTargetB, tcFlagIntent, "loto-in0v: work"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock b.go from the linked worktree failed")
	}
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", "loto-in0v: territory"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("claim from the linked worktree failed")
	}

	// Moved beside the old path so the repair below can name it relatively.
	moved := filepath.Join(filepath.Dir(wt), "fixer-b-moved")
	runGitIn(t, repo, "worktree", "move", wt, moved)
	moved = realpath(t, moved)
	t.Chdir(moved)

	// Plain `loto doctor`: reports the stale stamps, changes nothing, and
	// hands this owner the exact --moved-from command.
	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "stale_worktree_stamps=1") {
		t.Fatalf("doctor must report the stale worktree stamp count on line 1: %q", out)
	}
	if !strings.Contains(out, "⚠ stale_worktree_stamp old="+wt) {
		t.Fatalf("doctor must name the old path %q: %q", wt, out)
	}
	if !strings.Contains(out, "locks=1") || !strings.Contains(out, "claims=1") {
		t.Fatalf("the stale-stamp row must count both the lock and the claim: %q", out)
	}
	if !strings.Contains(out, "loto doctor --repair --moved-from "+shellQuote(wt)) {
		t.Fatalf("doctor must print the exact --moved-from fix for the caller's own stale path: %q", out)
	}

	// Bare --repair never adopts: a vanished path owned by this agent could
	// be a deleted sibling, not this checkout (PR #376 Codex P1).
	bareOut := runOK(t, tcCmdDoctor, "--repair")
	if strings.Contains(bareOut, "repaired-worktree-stamps") {
		t.Fatalf("--repair without --moved-from must not rewrite stamps: %q", bareOut)
	}

	// --dry-run names what --repair --moved-from would rewrite, writing nothing.
	dryOut := runOK(t, tcCmdDoctor, "--dry-run", "--moved-from", wt)
	if !strings.Contains(dryOut, "would_repair_worktree_stamps=2") {
		t.Fatalf("--dry-run --moved-from must count both rows: %q", dryOut)
	}
	if again := runOK(t, tcCmdDoctor); !strings.Contains(again, "⚠ stale_worktree_stamp old="+wt) {
		t.Fatalf("--dry-run must not write: %q", again)
	}

	// `loto doctor --repair --moved-from <old>` rewrites them; a relative,
	// unclean spelling of the old path names the same checkout.
	repairOut := runOK(t, tcCmdDoctor, "--repair", "--moved-from", "../fixer-b/")
	if !strings.Contains(repairOut, "✓ repaired-worktree-stamps count=2") {
		t.Fatalf("--repair --moved-from must report both rows rewritten: %q", repairOut)
	}

	// A second doctor run from the same (new) path is clean.
	out2 := runOK(t, tcCmdDoctor)
	if strings.Contains(out2, "⚠ stale_worktree_stamp") {
		t.Fatalf("doctor after repair must show no more stale stamps: %q", out2)
	}

	// `unlock --all` now releases both, with no ambiguity refusal.
	var unlockOut, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "loto-in0v: cleanup"}, &unlockOut, &errBuf); code != 0 {
		t.Fatalf("unlock --all after repair must succeed, got exit %d: out=%q err=%q", code, unlockOut.String(), errBuf.String())
	}
	if strings.Contains(errBuf.String(), "ambiguous") {
		t.Errorf("unlock --all must not refuse as ambiguous after repair: %q", errBuf.String())
	}
	if !strings.Contains(unlockOut.String(), "count=1") {
		t.Errorf("want the one lock released: %q", unlockOut.String())
	}
}

// TestDoctorRepair_TwoLiveWorktreesLeavesOtherUntouched is AC2 exercised
// through the CLI: two real, still-existing linked worktrees each hold their
// own lock. `loto doctor --repair` run from one must not touch the other's
// row — its worktree stamp is a live sibling, not a stale one, so it is never
// even a repair candidate (Rules: "Never rewrite a stamp whose path still
// exists").
func TestDoctorRepair_TwoLiveWorktreesLeavesOtherUntouched(t *testing.T) {
	repo, wt := siblingCheckouts(t) // cwd is repo (fixer-a); wt is the fixer-b linked worktree
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "fixer-a work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixer-a lock: exit %d", code)
	}

	// Give fixer-b its own file to lock (its checkout has no committed files).
	if err := os.WriteFile(filepath.Join(wt, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if code := Run([]string{tcCmdLock, tcTargetB, tcFlagIntent, "fixer-b work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixer-b lock: exit %d", code)
	}

	// --repair from fixer-b: only ever a candidate for ITS OWN rows anyway
	// (owner scoping), and neither worktree's path has moved, so nothing
	// should be rewritten on either side.
	repairOut := runOK(t, tcCmdDoctor, "--repair")
	if strings.Contains(repairOut, "repaired-worktree-stamps") {
		t.Fatalf("no worktree has moved; --repair must not rewrite anything: %q", repairOut)
	}

	// fixer-a's own doctor run, from its own still-live path, reports no
	// stale stamps — proof its row was not touched or invalidated by
	// fixer-b's --repair.
	t.Chdir(repo)
	docOut := runOK(t, tcCmdDoctor)
	if strings.Contains(docOut, "⚠ stale_worktree_stamp") {
		t.Fatalf("fixer-a's row must still read healthy after fixer-b's repair: %q", docOut)
	}
}

// TestDoctorRepair_MovedFromRequiresRepairFlag pins the usage guard added
// alongside --moved-from: naming a prior path only means something together
// with --repair or --dry-run.
func TestDoctorRepair_MovedFromRequiresRepairFlag(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdDoctor, "--moved-from", "/some/old/path"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("--moved-from without --repair must be refused, got exit 0: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "--moved-from") {
		t.Errorf("refusal must name --moved-from: %q", errBuf.String())
	}
}

// movedWorktreeWithLock builds loto-in0v's moved-checkout fixture: a linked
// worktree locks b.go, then is moved. Returns the old (stamped) path and the
// new one; cwd is left at the new path.
func movedWorktreeWithLock(t *testing.T) (oldPath, newPath string) {
	t.Helper()
	repo := withTempProject(t)
	pinAgent(t)
	runGitIn(t, repo, "checkout", "-q", "-b", "fixer-a")
	runGitIn(t, repo, "commit", "--allow-empty", "-q", "-m", "init")
	wt := filepath.Join(t.TempDir(), "fixer-b")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "fixer-b", wt)
	wt = realpath(t, wt)
	if err := os.WriteFile(filepath.Join(wt, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if code := Run([]string{tcCmdLock, tcTargetB, tcFlagIntent, "loto-in0v: work"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock b.go from the linked worktree failed")
	}
	moved := filepath.Join(t.TempDir(), "fixer-b-moved")
	runGitIn(t, repo, "worktree", "move", wt, moved)
	moved = realpath(t, moved)
	t.Chdir(moved)
	return wt, moved
}

// TestDoctorStaleStamp_OtherOwnersPathGetsNoFix (cubic P2 on #376): stamp
// repair is owner-scoped, so a caller who owns none of an old path's rows is
// not handed a --moved-from command that would change nothing; the row names
// the owners instead.
func TestDoctorStaleStamp_OtherOwnersPathGetsNoFix(t *testing.T) {
	oldPath, _ := movedWorktreeWithLock(t)
	owner := os.Getenv("LOTO_AGENT_ID")
	pinAgent(t) // a different agent now

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "⚠ stale_worktree_stamp old="+oldPath) {
		t.Fatalf("the stale row is still reported to every caller: %q", out)
	}
	if !strings.Contains(out, "owners="+owner) {
		t.Fatalf("the row must name its owner %q: %q", owner, out)
	}
	if strings.Contains(out, "--moved-from") {
		t.Fatalf("no --moved-from fix for a path the caller owns nothing under: %q", out)
	}
}

// TestDoctorRepair_MovedFromRefusedWithoutPinnedIdentity (Codex P2 on #376):
// an unpinned shell runs as a throwaway owner that can match no stamp, so
// --moved-from must refuse loudly rather than print ✓ and change nothing.
func TestDoctorRepair_MovedFromRefusedWithoutPinnedIdentity(t *testing.T) {
	oldPath, _ := movedWorktreeWithLock(t)
	t.Setenv("LOTO_AGENT_ID", "") // explicit-ephemeral: not pinned

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdDoctor, "--repair", "--moved-from", oldPath}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("want exit 2 without a pinned identity, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "LOTO_AGENT_ID") {
		t.Errorf("refusal must name how to pin an identity: %q", errBuf.String())
	}
	if strings.Contains(out.String(), "✓ repaired") {
		t.Errorf("must not report a repair it did not do: %q", out.String())
	}
}
