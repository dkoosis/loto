package render

import (
	"fmt"
	"io"
	"strings"
	"time"

	"loto/internal/store"
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

// EmitReports renders drift and change reports — the same line whether it is
// delivered at the addressee's next pre-hook or listed as undelivered by
// `loto status`. One rendering, because the field order IS the report's
// contract: §5 rows 4-7 name the observer's CALL and the spanning calls, and
// never say "X wrote f". Two renderings is how that drifts.
//
// header is the section's first line. showAddressee is set by `loto status`,
// which lists every owner's undelivered reports; a delivery names no
// addressee, since the reader is the addressee.
func EmitReports(w io.Writer, header string, reports []store.TreeReport, showAddressee bool) {
	if len(reports) == 0 {
		return
	}
	fmt.Fprintf(w, "⚠ %s count=%d\n", header, len(reports))
	cwd := getCwd()
	for i := range reports {
		fmt.Fprintln(w, "⚠ "+reportLine(&reports[i], cwd, showAddressee))
	}
	fmt.Fprintln(w, "```bash")
	fmt.Fprintln(w, "loto events --kind tree_change_reported")
	fmt.Fprintln(w, "```")
}

// reportLine is one report as a single keyed line. Keyed rather than columnar
// because the fields present depend on the rule — a drift event has no
// observing call, and only row 1 has a second holder to name.
func reportLine(r *store.TreeReport, cwd string, showAddressee bool) string {
	var b strings.Builder
	if showAddressee {
		fmt.Fprintf(&b, "to=%s ", holderTag(string(r.Addressee)))
	}
	fmt.Fprintf(&b, "path=%s rule=%s seq=%d",
		relToCwd(r.Event.Path, cwd), r.Event.Rule, r.Event.Seq)
	if r.Event.HolderPre != "" {
		fmt.Fprintf(&b, " holder=%s", holderTag(string(r.Event.HolderPre)))
	}
	if r.Event.Rule == store.TreeRuleRow1 && r.Event.HolderNow != "" {
		fmt.Fprintf(&b, " holder_now=%s", holderTag(string(r.Event.HolderNow)))
	}
	if r.Event.CallID != "" {
		fmt.Fprintf(&b, " observer=%s call=%s", holderTag(string(r.Event.Observer)), r.Event.CallID)
	}
	fmt.Fprintf(&b, " spanners=%s", dashIfEmpty(spannerList(r.Event.Spanners)))
	fmt.Fprintf(&b, " digest=%s->%s note=%q",
		dashIfEmpty(shortDigest(r.Event.DigestPre)), dashIfEmpty(shortDigest(r.Event.DigestPost)),
		r.Event.Note)
	return b.String()
}

// spannerList names each spanning call as owner:call_id — the owner because
// the report is about whose windows overlapped, the call id because one owner
// can have several open at once.
func spannerList(sp []store.TreeSpanner) string {
	if len(sp) == 0 {
		return ""
	}
	names := make([]string, 0, len(sp))
	for i := range sp {
		names = append(names, holderTag(string(sp[i].Owner))+":"+sp[i].CallID)
	}
	return strings.Join(names, ",")
}

// shortDigest trims a blob hash to the prefix a reader compares by eye. The
// full value is in the store; a report is read, not diffed.
func shortDigest(d string) string {
	const n = 12
	if len(d) > n {
		return d[:n]
	}
	return d
}
