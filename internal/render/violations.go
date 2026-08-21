package render

import (
	"fmt"
	"io"
	"sort"
	"time"
)

// ViolationRow is one unresolved violation as the CLI surfaces it: content in
// the working tree that differs from integration on a path nothing
// authorized. No culprit field — the sensor reads content, not writers.
type ViolationRow struct {
	ID          string
	Path        string
	ObservedAt  time.Time
	LeaseState  string
	Fingerprint string
}

// EmitViolations renders the full `loto violations` report: triage count
// first, one ✗ row per open violation, then the fix block. Rows are re-sorted
// path -> id defensively, independent of caller order
// (.claude/rules/design.md: same input, byte-identical output).
//
// An empty set prints an explicit ✓ header rather than nothing — silence from
// a contamination check looks exactly like a crash, and this one is read by
// an agent deciding whether it is safe to submit.
func EmitViolations(w io.Writer, rows []ViolationRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "✓ violations count=0")
		return
	}
	sortViolationRows(rows)
	fmt.Fprintf(w, "✗ violations count=%d\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "✗ path=%s id=%s state=%s observed=%s\n",
			r.Path, r.ID, r.LeaseState, r.ObservedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(w, "```bash")
	fmt.Fprintln(w, "git diff refs/loto/integration -- <path>   # look before you clear it")
	fmt.Fprintln(w, "git checkout refs/loto/integration -- <path>  # revert: auto-resolves")
	fmt.Fprintln(w, `loto violations resolve <id> -m "why this change is legitimate"`)
	fmt.Fprintln(w, "```")
}

// EmitViolationNotice is the one-line resurfacing other commands carry —
// `loto status`, `loto check --gate`, and ahead of a submit. Advisory, never
// blocking: the correctness verdict belongs to admission, and a notice that
// stopped work would make the sensor an outage.
//
// Silent on zero. Unlike EmitViolations, this rides along inside another
// command's output, where an unconditional "count=0" line would be noise on
// every tool call the PreToolUse gate sees.
func EmitViolationNotice(w io.Writer, rows []ViolationRow) {
	if len(rows) == 0 {
		return
	}
	sortViolationRows(rows)
	fmt.Fprintf(w, "⚠ violations count=%d unresolved=%s\n", len(rows), rows[0].Path)
	if len(rows) > 1 {
		fmt.Fprintf(w, "⚠ more=%d run=`loto violations`\n", len(rows)-1)
	}
}

func sortViolationRows(rows []ViolationRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].ID < rows[j].ID
	})
}
