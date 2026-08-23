package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"loto/internal/identity"
	"loto/internal/render"
)

// branchVerdict is what `loto check --branch` decided about one checkout that
// has the branch.
type branchVerdict struct {
	Worktree string
	PID      int    // 0 when the lock names none
	Holder   string // agent token from the lock reason, "" when unreadable
	// State is why this checkout does or does not block, in the vocabulary
	// the report prints: "live" | "unlocked" | "unreadable-lock" | "gone".
	State string
}

// Blocks reports whether this checkout refuses the publication.
//
// ‡ Everything except a provably-gone holder blocks. The action being gated
// is a sweeper publishing a branch it believes abandoned — push, PR, merge —
// and the failure mode is shipping another agent's mid-revision tree to main
// (loto-c1o3). A wrong refusal costs one look; a wrong green light costs work
// that cannot be recovered, because by then it is on main under someone
// else's name.
func (v branchVerdict) Blocks() bool { return v.State != branchStateGone }

const (
	branchStateLive       = "live"
	branchStateUnlocked   = "unlocked"
	branchStateUnreadable = "unreadable-lock"
	branchStateGone       = "gone"
)

// pidLive is the process-liveness oracle, swapped in tests. Deliberately the
// same primitive pidVerdict uses, so "is that session up" has one answer in
// this repo rather than two that can disagree.
//
//nolint:gochecknoglobals // test seam, mirrors identity's own killFn precedent
var pidLive = identity.PIDAlive

// decideBranch judges every checkout holding branch (a short name, e.g.
// "loto-ovno.11") and returns the verdicts in deterministic path order.
// self is the caller's own worktree path, which never blocks it.
//
// Pure but for the pid probe: fed parsed records, so the whole decision is
// testable without a git repo.
func decideBranch(recs []worktreeRec, branch, self string) []branchVerdict {
	want := "refs/heads/" + branch
	var out []branchVerdict
	for i := range recs {
		r := &recs[i]
		if r.Branch != want || sameWorktree(r.Path, self) {
			continue
		}
		out = append(out, judgeWorktree(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worktree < out[j].Worktree })
	return out
}

// judgeWorktree turns one holding checkout into a verdict.
func judgeWorktree(r *worktreeRec) branchVerdict {
	v := branchVerdict{Worktree: r.Path, Holder: lockReasonAgent(r.Reason)}
	switch {
	case !r.Locked:
		// Checked out and nobody said who by. git will still refuse to
		// delete it, and loto has no handle to prove the tree is idle.
		v.State = branchStateUnlocked
	default:
		pid, ok := lockReasonPID(r.Reason)
		if !ok {
			v.State = branchStateUnreadable
			return v
		}
		v.PID = pid
		if pidLive(pid) {
			v.State = branchStateLive
		} else {
			v.State = branchStateGone
		}
	}
	return v
}

// lockReasonAgent pulls the agent token out of a lock reason for the report.
// Cosmetic — it names who to go ask — and never load-bearing: an empty holder
// changes no verdict.
func lockReasonAgent(reason string) string {
	for f := range strings.FieldsSeq(reason) {
		if strings.HasPrefix(f, "agent-") {
			return f
		}
	}
	return ""
}

// sameWorktree compares two checkout paths. Cleaned but not symlink-resolved:
// git prints its own canonical path, and the caller's repoTop comes from
// git too, so the two already agree.
func sameWorktree(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// runCheckBranch answers "may I publish this branch?" — the check a sweeper
// owes before treating a local branch with no PR as abandoned work.
//
// ‡ This exists because git's own guardrail covers the wrong half. A locked
// worktree makes `git worktree remove` and `git branch -D` refuse, naming the
// holder; it has no opinion whatsoever about `git push`, `gh pr create`, or
// `gh pr merge`. A sweeper published and merged a branch out of a live
// worktree and git said nothing — it survived on ten minutes of timing
// (loto-c1o3).
//
// ‡ Publication routed through the gate needs none of this: `loto pr` builds
// branches from refs/loto/integration and never reads a working tree, so it
// structurally cannot publish unpromoted work. This is the backstop for
// branch-shaped publication, not a blessing of it.
func runCheckBranch(ctx context.Context, branch string, stdout, stderr io.Writer) int {
	if branch == "" {
		fmt.Fprintln(stderr, "✗ --branch needs a branch name")
		return 2
	}
	repoTop, err := repoTopForCwd(ctx)
	if err != nil || repoTop == "" {
		fmt.Fprintln(stderr, "✗ not inside a git repository")
		return 3
	}
	recs, err := gitWorktrees(ctx, repoTop)
	if err != nil {
		// No listing means no evidence, and no evidence is not permission.
		fmt.Fprintf(stderr, "✗ list worktrees: %v\n", err)
		return 3
	}

	verdicts := decideBranch(recs, branch, repoTop)
	rows := make([]render.BranchHolderRow, len(verdicts))
	blocked := 0
	for i, v := range verdicts {
		rows[i] = render.BranchHolderRow{
			Worktree: relativeToBase(repoTop, v.Worktree),
			State:    v.State, PID: v.PID, Holder: v.Holder, Blocks: v.Blocks(),
		}
		if v.Blocks() {
			blocked++
		}
	}
	render.EmitBranchCheck(stdout, branch, rows)
	if blocked > 0 {
		return 1
	}
	return 0
}

// relativeToBase shortens a worktree path against the repo when it sits
// inside it (.claude/worktrees/... is the common case), and leaves it
// absolute otherwise — a path outside the repo is not clearer for being
// rewritten as ../../.. (.claude/rules/design.md: relative paths when
// relative works).
func relativeToBase(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// flagWasSet reports whether name was given on the command line, as opposed
// to sitting at its zero value. flag has no accessor for this, so it walks
// the visited set — the standard idiom.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
