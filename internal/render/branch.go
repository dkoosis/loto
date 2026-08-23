package render

import (
	"fmt"
	"io"
)

// BranchHolderRow is one checkout that has the branch, and what loto could
// establish about whoever is in it.
type BranchHolderRow struct {
	Worktree string
	State    string
	PID      int
	Holder   string
	Blocks   bool
}

// EmitBranchCheck renders `loto check --branch`: the verdict on the first
// line, then one row per checkout holding the branch.
//
// ‡ The empty case prints an explicit ✓ line. A sweeper reads this to decide
// whether to publish, and silence is indistinguishable from a crashed check —
// which would read as permission (.claude/rules/design.md: explicit
// empty-status header).
func EmitBranchCheck(w io.Writer, branch string, rows []BranchHolderRow) {
	blocked := 0
	for _, r := range rows {
		if r.Blocks {
			blocked++
		}
	}
	glyph := "✓"
	if blocked > 0 {
		glyph = "✗"
	}
	fmt.Fprintf(w, "%s branch=%s holders=%d blocking=%d\n", glyph, branch, len(rows), blocked)
	for _, r := range rows {
		rowGlyph := "ℹ"
		if r.Blocks {
			rowGlyph = "✗"
		}
		fmt.Fprintf(w, "%s worktree=%s state=%s%s%s\n",
			rowGlyph, r.Worktree, r.State, pidField(r.PID), holderField(r.Holder))
	}
	if blocked == 0 {
		return
	}
	fmt.Fprintln(w, "```bash")
	fmt.Fprintln(w, "loto who                      # who to ask before touching it")
	fmt.Fprintln(w, "git worktree list             # where the checkout is")
	fmt.Fprintln(w, "```")
}

// pidField and holderField omit what the lock did not say, rather than
// printing pid=0 / holder= on every row. A field that is only ever noise
// when empty is not a field (.claude/rules/design.md).
func pidField(pid int) string {
	if pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" pid=%d", pid)
}

func holderField(holder string) string {
	if holder == "" {
		return ""
	}
	return " holder=" + holder
}
