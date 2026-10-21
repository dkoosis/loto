package render

import (
	"fmt"
	"io"
)

// PresenceRow is one other session working in, or possibly in, the worktree
// asked about.
type PresenceRow struct {
	Session string
	PID     int
	State   string // "occupied" | "unknown"
	Source  string // which record placed it: "loto" | "claude"
	At      string // the checkout or cwd that placed it
	Reason  string // why liveness or placement is unknown; empty otherwise
}

// EmitWorktreePresence renders `loto check --worktree`: the verdict and its
// counts on the first line, then one ✗ row per session that keeps the tree.
// Every row is a ✗ because every row means the same thing to the caller: do
// not delete this worktree.
//
// ‡ The free case prints an explicit ✓ line. A tool reads this to decide
// whether to delete a tree, and silence is indistinguishable from a crashed
// check (.claude/rules/design.md: explicit empty-status header).
func EmitWorktreePresence(w io.Writer, worktree string, occupied, unknown int, rows []PresenceRow) {
	verdict := "free"
	switch {
	case occupied > 0:
		verdict = "occupied"
	case unknown > 0:
		verdict = "unknown"
	}
	glyph := "✓"
	if verdict != "free" {
		glyph = "✗"
	}
	fmt.Fprintf(w, "%s worktree=%s %s occupied=%d unknown=%d\n", glyph, worktree, verdict, occupied, unknown)
	for _, r := range rows {
		fmt.Fprintf(w, "✗ session=%s state=%s source=%s%s at=%s%s\n",
			r.Session, r.State, r.Source, pidField(r.PID), r.At, reasonField(r.Reason))
	}
}

func reasonField(reason string) string {
	if reason == "" {
		return ""
	}
	return " reason=" + reason
}
