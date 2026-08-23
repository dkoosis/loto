package cli

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// worktreeRec is one record from `git worktree list --porcelain`: where a
// checkout lives, what it has checked out, and whether something has claimed
// it.
//
// ‡ The lock is the interesting field, and it is not loto's. Claude Code
// locks the worktrees it hands agents, and the reason it writes carries the
// pid of the owning session — which makes a foreign string the only handle
// loto has on "is that checkout still somebody's?". Parsed defensively for
// exactly that reason: a reason loto cannot read is UNKNOWN, never free.
type worktreeRec struct {
	Path   string
	HEAD   string
	Branch string // full ref (refs/heads/x); empty when detached or bare
	Locked bool
	Reason string // lock reason as git printed it; may be empty on a bare lock
	Bare   bool
}

// parseWorktreePorcelain reads `git worktree list --porcelain`: records
// separated by a blank line, each line either "key value" or a bare flag.
//
// Unknown keys are ignored rather than refused — git adds attributes
// (prunable, and whatever comes next) and a parser that fails on one it has
// not met would turn a routine git upgrade into a repo-wide refusal.
func parseWorktreePorcelain(out string) []worktreeRec {
	var recs []worktreeRec
	var cur *worktreeRec
	flush := func() {
		if cur != nil && cur.Path != "" {
			recs = append(recs, *cur)
		}
		cur = nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		if key == "worktree" {
			flush()
			cur = &worktreeRec{Path: val}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.HEAD = val
		case "branch":
			cur.Branch = val
		case "bare":
			cur.Bare = true
		case "locked":
			// `locked` alone is a lock with no reason — still a lock.
			cur.Locked = true
			cur.Reason = val
		}
	}
	flush()
	return recs
}

// lockReasonPID pulls the pid out of a worktree lock reason.
//
// Claude Code writes "claude agent agent-<id> (pid 44312 start Thu Aug 20
// 19:21:35 2026)". Nothing guarantees that shape — it is another tool's
// prose — so this looks for the "pid <digits>" token and reports failure
// rather than guessing. A reason with no pid yields ok=false, which the
// caller must treat as UNKNOWN: an unreadable claim is not an absent one.
//
// ‡ The start-time half of that reason is deliberately NOT consulted. It
// would only ever be used to argue that a live pid is a DIFFERENT process
// than the one recorded, i.e. to turn a refusal into permission — and this
// check gates publishing another agent's unfinished work, where a wrong
// green light is the one unrecoverable outcome. Over-refusing on a reused
// pid costs one manual look.
func lockReasonPID(reason string) (int, bool) {
	fields := strings.Fields(reason)
	for i, f := range fields {
		// The token arrives punctuated — Claude Code writes "(pid 44312 ..."
		// — so the key is trimmed too, not just the value. Reading only a
		// bare "pid" is what made a perfectly readable lock report as
		// unreadable the first time this ran against a real worktree.
		if strings.Trim(f, "([{") != "pid" || i+1 >= len(fields) {
			continue
		}
		digits := strings.Trim(fields[i+1], "()[]{},.;:")
		if pid, err := strconv.Atoi(digits); err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

// gitWorktrees lists the checkouts of repoTop.
func gitWorktrees(ctx context.Context, repoTop string) ([]worktreeRec, error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = repoTop
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(string(out)), nil
}
