package render

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const (
	llmHeader    = "loto:llm:v1\n"
	intentMax    = 80
	inboxBodyMax = 200
	shortIDLen   = 8
	// emptyStatus is the explicit placeholder for empty result sets.
	// Silence is dangerous — looks like a crash to the agent reading output.
	emptyStatus = "[status: empty]"
	// dash is the placeholder for absent optional column values.
	dash = "-"
	// tokenHintThreshold is the row count above which an emitter writes a
	// token-estimate hint after the header. Below this, the hint is noise.
	tokenHintThreshold = 20
	// avgTokensPerRow is a rough heuristic for estimating LLM token cost
	// per output row. Tuned against typical inbox/reserve/doctor lines.
	avgTokensPerRow = 25
)

// RelPath returns p as a path relative to cwd when p resolves under cwd; on
// any error, or when p escapes cwd ("../"), it returns the original input.
// Used to keep emitter output cwd-relative — agents see paths the way they
// already address them.
func RelPath(p string) string {
	if p == "" {
		return p
	}
	cwd, err := filepathAbs(".")
	if err != nil {
		return p
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

// filepathAbs is a seam for testing; defaults to filepath.Abs.
var filepathAbs = filepath.Abs

// writeTokenHint emits a single comment line of the form
// "# est_tokens:~N rows:M" when rows exceeds tokenHintThreshold.
// Appears immediately after the header/count line so agents can budget
// before consuming the body.
func writeTokenHint(w io.Writer, rows int) error {
	if rows <= tokenHintThreshold {
		return nil
	}
	_, err := fmt.Fprintf(w, "# est_tokens:~%d rows:%d\n", rows*avgTokensPerRow, rows)
	return err
}

// writeHeader emits the version sentinel that prefixes every LLM payload.
func writeHeader(w io.Writer) error {
	_, err := io.WriteString(w, llmHeader)
	return err
}

// rfc3339UTC formats t in the canonical UTC representation used across all
// LLM output. Centralized so timestamp shape can't drift between emitters.
func rfc3339UTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// shortID returns a compact 8-char display form. For shell-keyed IDs the
// "shell-" prefix is dropped first so the disambiguating hex bytes survive
// the truncation; UUIDs and arbitrary IDs are truncated as-is.
func shortID(id string) string {
	core := strings.TrimPrefix(id, "shell-")
	if len(core) <= shortIDLen {
		return core
	}
	return core[:shortIDLen]
}

// orDash returns s, or "-" when s is empty. Used for optional columns in
// fixed-shape table rows where a literal dash signals "absent".
func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// appendTTL appends " | ttl:<rfc3339>" when t is non-zero.
func appendTTL(b *strings.Builder, t time.Time) {
	if !t.IsZero() {
		fmt.Fprintf(b, " | ttl:%s", rfc3339UTC(t))
	}
}

// appendIntent appends " | intent:<truncated>" when s is non-empty.
func appendIntent(b *strings.Builder, s string) {
	if s != "" {
		fmt.Fprintf(b, " | intent:%s", truncIntent(s))
	}
}

// EmitLLMWhoami writes the whoami output in LLM format.
// Layout: "agent | <handle> | id:<short> | host:<host>"
func EmitLLMWhoami(w io.Writer, id, handle, host string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "agent | %s | id:%s | host:%s\n", handle, shortID(id), host)
	return err
}

// ReservationWarning describes a soft conflict surfaced after a successful acquire.
type ReservationWarning struct {
	Pattern string
	AgentID string
}

// BlockedInput is the data needed to render a holder-blocked report.
type BlockedInput struct {
	Kind      string // "file" | "global"
	Target    string
	AgentID   string
	Intent    string
	HeldSince time.Time
	ExpiresAt time.Time // zero = no TTL
	Branch    string
	Host      string
	PID       int
}

func truncIntent(s string) string {
	if len(s) <= intentMax {
		return s
	}
	return s[:intentMax-1] + "…"
}

// EmitLLMTrySuccess writes a one-line acquired record plus optional reservation warnings.
func EmitLLMTrySuccess(w io.Writer, kind, target, agent string, warnings []ReservationWarning) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "✔ acquired | %s | %s | by:%s\n", kind, target, agent); err != nil {
		return err
	}
	for _, rw := range warnings {
		if _, err := fmt.Fprintf(w, "⚠ reservation | %s | held-by:%s\n", rw.Pattern, rw.AgentID); err != nil {
			return err
		}
	}
	return nil
}

// EmitLLMBlocked writes a holder-blocked report. Optional fields are appended
// only when non-zero.
func EmitLLMBlocked(w io.Writer, in BlockedInput) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✗ blocked | %s | %s | by:%s | intent:%s | held-since:%s",
		in.Kind, in.Target, in.AgentID, truncIntent(in.Intent), rfc3339UTC(in.HeldSince))
	appendTTL(&b, in.ExpiresAt)
	if in.Branch != "" {
		fmt.Fprintf(&b, " | branch:%s", in.Branch)
	}
	if in.Host != "" {
		fmt.Fprintf(&b, " | host:%s", in.Host)
	}
	if in.PID != 0 {
		fmt.Fprintf(&b, " | pid:%d", in.PID)
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// StatusEntry is one row of `loto status <targets...>` output.
type StatusEntry struct {
	Target  string
	Free    bool
	AgentID string
	Intent  string
}

// EmitLLMStatusGlobal writes the global-lock status line.
func EmitLLMStatusGlobal(w io.Writer, free bool, agent, intent string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if free {
		_, err := io.WriteString(w, "✔ global | free\n")
		return err
	}
	_, err := fmt.Fprintf(w, "✗ global | by:%s | intent:%s\n", agent, intent)
	return err
}

// InboxMessage is a single message rendered for `loto inbox`.
type InboxMessage struct {
	From string
	To   string
	Body string
}

func collapseBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= inboxBodyMax {
		return s
	}
	return s[:inboxBodyMax-1] + "…"
}

// EmitLLMInbox writes inbox content. Empty inbox renders an explicit
// emptyStatus line — see emptyStatus for why.
func EmitLLMInbox(w io.Writer, target string, msgs []InboxMessage) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if len(msgs) == 0 {
		_, err := fmt.Fprintf(w, "inbox | %s | %s\n", target, emptyStatus)
		return err
	}
	if _, err := fmt.Fprintf(w, "inbox | %s | %d msgs\n", target, len(msgs)); err != nil {
		return err
	}
	for _, m := range msgs {
		if _, err := fmt.Fprintf(w, "→ from:%s | to:%s | %s\n", m.From, m.To, collapseBody(m.Body)); err != nil {
			return err
		}
	}
	return nil
}

// InboxMineMessage is one row of the cross-mailbox `loto inbox --mine` view.
type InboxMineMessage struct {
	From      string
	To        string
	Target    string
	Timestamp time.Time
	Body      string
}

// EmitLLMInboxMine writes a cross-mailbox inbox view for the current agent.
// Empty inbox renders an explicit emptyStatus line — see emptyStatus.
func EmitLLMInboxMine(w io.Writer, msgs []InboxMineMessage, since time.Time) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	header := fmt.Sprintf("inbox-mine | n:%d", len(msgs))
	if !since.IsZero() {
		header += " | since:" + rfc3339UTC(since)
	}
	if len(msgs) == 0 {
		_, err := fmt.Fprintf(w, "%s | %s\n", header, emptyStatus)
		return err
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if err := writeTokenHint(w, len(msgs)); err != nil {
		return err
	}
	for _, m := range msgs {
		if _, err := fmt.Fprintf(w, "→ from:%s | to:%s | target:%s | ts:%s | %s\n",
			m.From, m.To, orDash(m.Target), rfc3339UTC(m.Timestamp),
			collapseBody(m.Body)); err != nil {
			return err
		}
	}
	return nil
}

// EmitLLMMsgSent confirms a sent message.
func EmitLLMMsgSent(w io.Writer, target, to string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ msg-sent | target:%s | to:%s\n", target, to)
	return err
}

// EmitLLMReleased reports release results: count + per-error lines.
func EmitLLMReleased(w io.Writer, agent string, n int, errs []string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "✔ released | agent:%s | n:%d\n", agent, n); err != nil {
		return err
	}
	for _, e := range errs {
		if _, err := fmt.Fprintf(w, "✗ release-error | %s\n", e); err != nil {
			return err
		}
	}
	return nil
}

// EmitLLMReaped confirms a reap.
func EmitLLMReaped(w io.Writer, target string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ reaped | %s\n", target)
	return err
}

// EmitLLMBroken confirms a forced break.
func EmitLLMBroken(w io.Writer, target, by, reason string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ broken | %s | by:%s | reason:%s\n", target, by, reason)
	return err
}

// EmitLLMError writes a typed error line: "✗ error | <op> | <reason>".
// Reasons are line-collapsed so a multi-line wrapped error can't break the row.
func EmitLLMError(w io.Writer, op, reason string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✗ error | %s | %s\n", op, collapseLine(reason))
	return err
}

// EmitLLMInstalled confirms a hook install.
func EmitLLMInstalled(w io.Writer, path string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ installed | %s\n", path)
	return err
}

// ReservationEntry is one row of `loto reserve list` output.
type ReservationEntry struct {
	Pattern   string
	AgentID   string
	Intent    string
	ExpiresAt time.Time // zero = no TTL
}

// writeReservationRow emits a single reservation line with the given prefix
// (e.g. "✔ reserved" or "→") followed by pattern, agent, optional intent, ttl.
func writeReservationRow(w io.Writer, prefix string, e ReservationEntry) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s | by:%s", prefix, e.Pattern, e.AgentID)
	appendIntent(&b, e.Intent)
	appendTTL(&b, e.ExpiresAt)
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// EmitLLMReserveAdded confirms an advisory reservation was added.
func EmitLLMReserveAdded(w io.Writer, e ReservationEntry) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	return writeReservationRow(w, "✔ reserved |", e)
}

// EmitLLMReserveReleased confirms an advisory reservation was removed.
func EmitLLMReserveReleased(w io.Writer, pattern string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ unreserved | %s\n", pattern)
	return err
}

// EmitLLMReserveList writes the active reservations. Empty list renders an
// explicit emptyStatus line — see emptyStatus.
func EmitLLMReserveList(w io.Writer, entries []ReservationEntry) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintf(w, "reservations | %s\n", emptyStatus)
		return err
	}
	if _, err := fmt.Fprintf(w, "reservations | n:%d\n", len(entries)); err != nil {
		return err
	}
	if err := writeTokenHint(w, len(entries)); err != nil {
		return err
	}
	for _, e := range entries {
		if err := writeReservationRow(w, "→", e); err != nil {
			return err
		}
	}
	return nil
}

// DoctorFinding is one row of `loto doctor` LLM output. Mirrors loto.Finding
// in the public package; defined here to keep render free of a loto-package
// import cycle.
type DoctorFinding struct {
	Class       string // drift class (e.g. "stale_tag", "dead_pid")
	Path        string // tag path (the artifact on disk)
	Target      string // optional: target the lock guards
	AgentID     string // last holder, when known
	Detail      string // human-ish explanation from the doctor
	Repaired    bool   // DoctorRepair succeeded
	WouldRepair bool   // DoctorDryRun: would repair if --repair were set
	Error       string // DoctorRepair attempted but failed; explains why
}

// doctorClassGlyph maps a drift class to its severity glyph.
// Action-required classes get ✗; report-only get ⚠ or ℹ.
func doctorClassGlyph(class string) string {
	switch class {
	case "stale_tag", "dead_pid", "orphaned", "zombie_held":
		return "✗"
	case "soft_stale_held":
		return "⚠"
	case "layout_drift":
		return "ℹ"
	default:
		return "ℹ"
	}
}

// doctorClassRepairable reports whether `loto doctor --repair` knows how to
// clear this class. Used to decide whether to emit an inline fix command.
func doctorClassRepairable(class string) bool {
	switch class {
	case "stale_tag", "dead_pid", "orphaned", "zombie_held":
		return true
	}
	return false
}

// doctorTriage returns the first-line counts (e.g. "3 ✗ 1 ⚠ 0 ℹ").
func doctorTriage(findings []DoctorFinding) string {
	var errs, warns, info int
	for _, f := range findings {
		switch doctorClassGlyph(f.Class) {
		case "✗":
			errs++
		case "⚠":
			warns++
		default:
			info++
		}
	}
	return fmt.Sprintf("%d ✗ %d ⚠ %d ℹ", errs, warns, info)
}

// EmitLLMDoctor writes the doctor report in LLM format. Empty findings render
// an explicit ok header — see emptyStatus for why silence is dangerous.
// Findings should already be sorted deterministically by the caller.
func EmitLLMDoctor(w io.Writer, findings []DoctorFinding, mode string) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if len(findings) == 0 {
		_, err := fmt.Fprintf(w, "doctor | mode:%s | %s\n", mode, "[status: ok]")
		return err
	}
	if _, err := fmt.Fprintf(w, "%s | doctor | mode:%s\n", doctorTriage(findings), mode); err != nil {
		return err
	}
	if err := writeTokenHint(w, len(findings)); err != nil {
		return err
	}
	for _, f := range findings {
		if err := writeDoctorRow(w, f); err != nil {
			return err
		}
	}
	return nil
}

// writeDoctorRow emits one finding plus its repair-state suffix and (when
// applicable) an inline fix command.
func writeDoctorRow(w io.Writer, f DoctorFinding) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s | %s", doctorClassGlyph(f.Class), f.Class, f.Path)
	if f.Target != "" && f.Target != f.Path {
		fmt.Fprintf(&b, " | target:%s", f.Target)
	}
	if f.AgentID != "" {
		fmt.Fprintf(&b, " | by:%s", f.AgentID)
	}
	switch {
	case f.Repaired:
		b.WriteString(" | repaired:yes")
	case f.WouldRepair:
		b.WriteString(" | would-repair:yes")
	case f.Error != "":
		fmt.Fprintf(&b, " | repair-failed:%s", collapseLine(f.Error))
	}
	fmt.Fprintf(&b, " | %s\n", collapseLine(f.Detail))
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	// Fix block only when there's still work for the agent to do.
	if !f.Repaired && !f.WouldRepair && doctorClassRepairable(f.Class) {
		if _, err := fmt.Fprintf(w, "```bash\nloto doctor --repair\n```\n"); err != nil {
			return err
		}
	}
	return nil
}

// collapseLine flattens newlines so a multiline detail can't break the row format.
func collapseLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// CheckPathsConflict is one row of `loto check-paths` LLM output. Mirrors
// the cmd-side pathConflict struct; defined here to keep render free of a
// loto-package import cycle.
type CheckPathsConflict struct {
	Kind    string // "lock" | "reservation"
	Path    string
	Holder  string
	Intent  string
	Pattern string // reservation only
}

// EmitLLMCheckPaths writes a path-conflict report in LLM format. Conflicts
// should already be sorted deterministically by the caller. Caller decides
// whether to emit nothing on empty input — typical pre-commit hook flow only
// calls this when blocking.
func EmitLLMCheckPaths(w io.Writer, conflicts []CheckPathsConflict) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if len(conflicts) == 0 {
		_, err := fmt.Fprintf(w, "check-paths | %s\n", "[status: ok]")
		return err
	}
	var locks, reservations int
	for _, c := range conflicts {
		if c.Kind == "reservation" {
			reservations++
		} else {
			locks++
		}
	}
	if _, err := fmt.Fprintf(w, "%d ✗ | check-paths | locks:%d | reservations:%d\n",
		len(conflicts), locks, reservations); err != nil {
		return err
	}
	for _, c := range conflicts {
		if err := writeCheckPathsRow(w, c); err != nil {
			return err
		}
	}
	return nil
}

// writeCheckPathsRow emits one conflict line plus its inline fix command.
func writeCheckPathsRow(w io.Writer, c CheckPathsConflict) error {
	var b strings.Builder
	fmt.Fprintf(&b, "✗ %s | %s", c.Kind, c.Path)
	if c.Pattern != "" {
		fmt.Fprintf(&b, " | pattern:%s", c.Pattern)
	}
	fmt.Fprintf(&b, " | by:%s", c.Holder)
	appendIntent(&b, c.Intent)
	b.WriteByte('\n')
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if c.Kind == "reservation" {
		_, err := fmt.Fprintf(w, "```bash\nloto reserve release %s\n```\n", c.Pattern)
		return err
	}
	_, err := fmt.Fprintf(w, "```bash\nloto break --force %s --reason \"pre-commit\"\n```\n", c.Path)
	return err
}

// EmitLLMStatusTargets writes a small positional table for per-file status.
func EmitLLMStatusTargets(w io.Writer, entries []StatusEntry) error {
	if err := writeHeader(w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "status | target | holder | intent\n"); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Free {
			if _, err := fmt.Fprintf(w, "✔ free | %s | - | -\n", e.Target); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "✗ held | %s | %s | %s\n", e.Target, e.AgentID, orDash(e.Intent)); err != nil {
			return err
		}
	}
	return nil
}
