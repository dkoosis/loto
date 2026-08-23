package cli

import (
	"strings"
	"testing"

	"loto/internal/render"
)

const (
	tcShortBranch = "loto-ovno.11"
	tcBranchName  = "loto-ovno.11-gate-contract-tests"
	tcSelfPath    = "/repo"
	tcAgentPath   = "/repo/.claude/worktrees/agent-a88"
)

// withPidLive swaps the process oracle for one test.
func withPidLive(t *testing.T, alive bool) {
	t.Helper()
	prev := pidLive
	pidLive = func(int) bool { return alive }
	t.Cleanup(func() { pidLive = prev })
}

func holding(path, reason string, locked bool) worktreeRec {
	return worktreeRec{Path: path, Branch: "refs/heads/" + tcBranchName, Locked: locked, Reason: reason}
}

// The incident this exists for: a sweeper treats a branch with no PR as
// abandoned while an agent is still committing to it in a live worktree
// (loto-c1o3).
func TestDecideBranch_LiveHolderBlocks(t *testing.T) {
	withPidLive(t, true)
	got := decideBranch([]worktreeRec{
		holding(tcAgentPath, "claude agent agent-a88 (pid 44312 start Thu Aug 20 19:21:35 2026)", true),
	}, tcBranchName, tcSelfPath)
	if len(got) != 1 {
		t.Fatalf("want 1 verdict, got %+v", got)
	}
	if got[0].State != branchStateLive || !got[0].Blocks() {
		t.Errorf("live holder does not block: %+v", got[0])
	}
	if got[0].PID != 44312 || got[0].Holder != "agent-a88" {
		t.Errorf("holder not named: %+v", got[0])
	}
}

// A provably-gone holder is the ONLY state that clears. Everything else is
// refused, because the wrong green light ships another agent's tree to main.
func TestDecideBranch_OnlyAGoneHolderClears(t *testing.T) {
	tests := []struct {
		name      string
		rec       worktreeRec
		alive     bool
		wantState string
		wantBlock bool
	}{
		{"holder up", holding(tcAgentPath, "(pid 44312)", true), true, branchStateLive, true},
		{"holder gone", holding(tcAgentPath, "(pid 44312)", true), false, branchStateGone, false},
		{"lock names no pid", holding(tcAgentPath, "do not touch", true), false, branchStateUnreadable, true},
		{"checked out, unlocked", holding(tcAgentPath, "", false), false, branchStateUnlocked, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withPidLive(t, tt.alive)
			got := decideBranch([]worktreeRec{tt.rec}, tcBranchName, tcSelfPath)
			if len(got) != 1 {
				t.Fatalf("want 1 verdict, got %+v", got)
			}
			if got[0].State != tt.wantState || got[0].Blocks() != tt.wantBlock {
				t.Errorf("state=%q blocks=%v, want %q/%v", got[0].State, got[0].Blocks(), tt.wantState, tt.wantBlock)
			}
		})
	}
}

// A branch nobody has checked out is free, and so is one only the caller's
// own checkout holds — otherwise every agent would be refused its own branch.
func TestDecideBranch_UnheldAndSelfHeldAreFree(t *testing.T) {
	withPidLive(t, true)
	recs := []worktreeRec{
		{Path: tcSelfPath, Branch: "refs/heads/" + tcBranchName},
		{Path: "/repo/.claude/worktrees/other", Branch: "refs/heads/unrelated", Locked: true, Reason: "(pid 1)"},
	}
	if got := decideBranch(recs, tcBranchName, tcSelfPath); len(got) != 0 {
		t.Errorf("self-held branch blocked its own checkout: %+v", got)
	}
	if got := decideBranch(recs, "never-checked-out", tcSelfPath); len(got) != 0 {
		t.Errorf("unheld branch reported a holder: %+v", got)
	}
}

// A branch name that is a prefix of a real one must not match it — refusing
// `feature` because `feature-2` is checked out would be a lie.
func TestDecideBranch_MatchesTheWholeRefNotAPrefix(t *testing.T) {
	withPidLive(t, true)
	recs := []worktreeRec{holding(tcAgentPath, "(pid 1)", true)}
	if got := decideBranch(recs, tcShortBranch, tcSelfPath); len(got) != 0 {
		t.Errorf("prefix matched a longer branch: %+v", got)
	}
}

// Two checkouts of one branch report in path order, so the same repo state
// renders byte-identically (.claude/rules/design.md).
func TestDecideBranch_SortsByWorktreePath(t *testing.T) {
	withPidLive(t, true)
	got := decideBranch([]worktreeRec{
		holding("/repo/wt/z", "(pid 1)", true),
		holding("/repo/wt/a", "(pid 2)", true),
	}, tcBranchName, tcSelfPath)
	if len(got) != 2 || got[0].Worktree != "/repo/wt/a" {
		t.Fatalf("not sorted by path: %+v", got)
	}
}

// The report never goes silent: a sweeper reads it to decide whether to
// publish, and silence would read as permission.
func TestEmitBranchCheck_EmptyPrintsAnExplicitClear(t *testing.T) {
	var b strings.Builder
	render.EmitBranchCheck(&b, "x", nil)
	if !strings.Contains(b.String(), "✓ branch=x holders=0 blocking=0") {
		t.Errorf("empty report is not explicit: %q", b.String())
	}
}

// Fields the lock did not supply are omitted rather than printed as pid=0.
func TestEmitBranchCheck_OmitsWhatTheLockDidNotSay(t *testing.T) {
	var b strings.Builder
	render.EmitBranchCheck(&b, "x", []render.BranchHolderRow{
		{Worktree: "wt/a", State: branchStateUnlocked, Blocks: true},
	})
	out := b.String()
	if strings.Contains(out, "pid=") || strings.Contains(out, "holder=") {
		t.Errorf("empty fields printed: %q", out)
	}
	if !strings.Contains(out, "✗ worktree=wt/a state=unlocked\n") {
		t.Errorf("row wrong: %q", out)
	}
}

// An unset shell variable must not read as permission. `loto check --branch
// "$BRANCH"` with BRANCH unset once fell through to the path check, found no
// paths, and printed "✓ no paths" — a green light to publish, handed out
// because the caller's expansion was empty.
func TestCheckBranch_EmptyBranchIsRefusedNotCleared(t *testing.T) {
	stdout, stderr, code := executeCommand(tcCmdCheck, tcFlagBranch, "")
	if code != 2 {
		t.Errorf("exit=%d, want 2 (usage refusal)", code)
	}
	out := stdout + stderr
	if strings.Contains(out, "✓") {
		t.Errorf("an empty --branch reported success: %q", out)
	}
	if !strings.Contains(out, "--branch needs a branch name") {
		t.Errorf("refusal does not say why: %q", out)
	}
}

// --branch answers a different question than the path flags; combining them
// means a caller who will be answered about the other one.
func TestCheckBranch_RefusesPathFlagsAlongsideIt(t *testing.T) {
	for _, args := range [][]string{
		{tcCmdCheck, tcFlagBranch, "x", tcFlagGate},
		{tcCmdCheck, tcFlagBranch, "x", tcFlagStaged},
		{tcCmdCheck, tcFlagBranch, "x", "some/path.go"},
	} {
		stdout, stderr, code := executeCommand(args...)
		if code != 2 {
			t.Errorf("%v: exit=%d, want 2; out=%q", args, code, stdout+stderr)
		}
	}
}

// The two obvious ways a sweeper gets a branch into a variable —
// `git symbolic-ref HEAD` and `git for-each-ref` — both emit the full ref.
// Before normalization it matched nothing, the report said holders=0, and
// the caller read a clean bill of health for a branch an agent was in.
func TestNormalizeBranch(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"short name", tcShortBranch, tcShortBranch, false},
		{"what git symbolic-ref emits", branchRefPrefix + tcShortBranch, tcShortBranch, false},
		{"a slashed branch name survives", branchRefPrefix + "loto/c1o3", "loto/c1o3", false},
		{"refs/heads/ with nothing after it", branchRefPrefix, "", true},
		{"a remote ref is not a local branch", "refs/remotes/origin/main", "", true},
		{"a tag is not a branch", "refs/tags/v1", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeBranch(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeBranch(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("normalizeBranch(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// End to end through the command: a ref shape the gate cannot resolve exits
// 2 and never prints a ✓, because a ✓ is permission to publish.
func TestCheckBranch_UnresolvableRefIsRefusedNotCleared(t *testing.T) {
	stdout, stderr, code := executeCommand(tcCmdCheck, tcFlagBranch, "refs/remotes/origin/main")
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if strings.Contains(stdout+stderr, "✓") {
		t.Errorf("an unresolvable ref reported success: %q", stdout+stderr)
	}
}
