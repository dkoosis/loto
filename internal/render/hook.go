package render

import (
	"fmt"
	"io"
	"time"
)

// EmitHookRefusal renders the one refusal `loto hook pre` can produce: I2
// admission said no, because the Edit-family file_path is locked by another
// owner (enforcement-design.md §5 I2, "admitted iff L(f) ∈ {s, ⊥}").
//
// Shaped like EmitGateDeny on purpose — the two are the same event seen at
// two interfaces, and a reader should not have to learn a second vocabulary
// for "who is blocking me". The holder is named because a refusal a caller
// cannot act on is worse than no refusal at all.
func EmitHookRefusal(w io.Writer, path, holderUUID, intent string, expiresAt time.Time) {
	cwd := getCwd()
	rel := relToCwd(path, cwd)
	fmt.Fprintln(w, "✗ refused count=1")
	fmt.Fprintf(w, "✗ path=%s kind=%s blocker=%s intent=%q expires_at=%s\n",
		rel, GateKindLock, holderTag(holderUUID), intent, expiresAt.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "ℹ the edit was not admitted; nothing was written and no call was recorded")
	fmt.Fprintln(w, "```bash")
	fmt.Fprintf(w, "loto tag %s \"<bead>: need this file\"\n", rel)
	fmt.Fprintln(w, "```")
}

// EmitEvents renders the store's audit rows for `loto events`. Newest last —
// an event log reads forward — and the store's own (created_at, id) order is
// preserved, so the same window renders byte-identically.
//
// One header line carries the triage count and the field names; the rows carry
// values only, since repeating a field name per row is exactly what
// .claude/rules/design.md forbids.
func EmitEvents(w io.Writer, rows []EventRow, kindFilter string, total int) {
	if kindFilter != "" {
		fmt.Fprintf(w, "ℹ events=%d shown=%d kind=%s\n", total, len(rows), kindFilter)
	} else {
		fmt.Fprintf(w, "ℹ events=%d shown=%d\n", total, len(rows))
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "ℹ when\tkind\tactor\ttarget\treason\tdetail")
	cwd := getCwd()
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(w, "ℹ %s\t%s\t%s\t%s\t%s\t%s\n",
			r.CreatedAt.UTC().Format(time.RFC3339), r.Kind, holderTag(r.ActorUUID),
			dashIfEmpty(relToCwd(r.Target, cwd)), dashIfEmpty(r.Reason), dashIfEmpty(r.Detail))
	}
}

// EventRow is one audit row as `loto events` prints it.
type EventRow struct {
	CreatedAt time.Time
	Kind      string
	ActorUUID string
	Target    string
	Reason    string
	Detail    string
}

// dashIfEmpty keeps the column count fixed. An empty field would collapse two
// tabs together and shift every later column for that row alone.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
