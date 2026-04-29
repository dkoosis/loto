package render

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const llmHeader = "loto:llm:v1\n"

// shortID returns a compact 8-char display form. For shell-keyed IDs the
// "shell-" prefix is dropped first so the disambiguating hex bytes survive
// the truncation; UUIDs and arbitrary IDs are truncated as-is.
func shortID(id string) string {
	const n = 8
	core := strings.TrimPrefix(id, "shell-")
	if len(core) <= n {
		return core
	}
	return core[:n]
}

// EmitLLMWhoami writes the whoami output in LLM format.
// Layout: "agent | <handle> | id:<short> | host:<host>"
func EmitLLMWhoami(w io.Writer, id, handle, host string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
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

const intentMax = 80

func truncIntent(s string) string {
	if len(s) <= intentMax {
		return s
	}
	return s[:intentMax-1] + "…"
}

// EmitLLMTrySuccess writes a one-line acquired record plus optional reservation warnings.
func EmitLLMTrySuccess(w io.Writer, kind, target, agent string, warnings []ReservationWarning) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
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
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✗ blocked | %s | %s | by:%s | intent:%s | held-since:%s",
		in.Kind, in.Target, in.AgentID, truncIntent(in.Intent),
		in.HeldSince.UTC().Format(time.RFC3339))
	if !in.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, " | ttl:%s", in.ExpiresAt.UTC().Format(time.RFC3339))
	}
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
	if _, err := io.WriteString(w, llmHeader); err != nil {
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

const inboxBodyMax = 200

func collapseBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= inboxBodyMax {
		return s
	}
	return s[:inboxBodyMax-1] + "…"
}

// EmitLLMInbox writes inbox content. Empty inbox renders an explicit
// `[status: empty]` line — silence is dangerous (looks like a crash).
func EmitLLMInbox(w io.Writer, target string, msgs []InboxMessage) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if len(msgs) == 0 {
		_, err := fmt.Fprintf(w, "inbox | %s | [status: empty]\n", target)
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

// EmitLLMMsgSent confirms a sent message.
func EmitLLMMsgSent(w io.Writer, target, to string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ msg-sent | target:%s | to:%s\n", target, to)
	return err
}

// EmitLLMReleased reports release results: count + per-error lines.
func EmitLLMReleased(w io.Writer, agent string, n int, errs []string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
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
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ reaped | %s\n", target)
	return err
}

// EmitLLMBroken confirms a forced break.
func EmitLLMBroken(w io.Writer, target, by, reason string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ broken | %s | by:%s | reason:%s\n", target, by, reason)
	return err
}

// EmitLLMInstalled confirms a hook install.
func EmitLLMInstalled(w io.Writer, path string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
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

// EmitLLMReserveAdded confirms an advisory reservation was added.
func EmitLLMReserveAdded(w io.Writer, e ReservationEntry) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✔ reserved | %s | by:%s", e.Pattern, e.AgentID)
	if e.Intent != "" {
		fmt.Fprintf(&b, " | intent:%s", truncIntent(e.Intent))
	}
	if !e.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, " | ttl:%s", e.ExpiresAt.UTC().Format(time.RFC3339))
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// EmitLLMReserveReleased confirms an advisory reservation was removed.
func EmitLLMReserveReleased(w io.Writer, pattern string) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "✔ unreserved | %s\n", pattern)
	return err
}

// EmitLLMReserveList writes the active reservations. Empty list renders an
// explicit `[status: empty]` line — silence is dangerous (looks like a crash).
func EmitLLMReserveList(w io.Writer, entries []ReservationEntry) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
		return err
	}
	if len(entries) == 0 {
		_, err := io.WriteString(w, "reservations | [status: empty]\n")
		return err
	}
	if _, err := fmt.Fprintf(w, "reservations | n:%d\n", len(entries)); err != nil {
		return err
	}
	for _, e := range entries {
		var b strings.Builder
		fmt.Fprintf(&b, "→ %s | by:%s", e.Pattern, e.AgentID)
		if e.Intent != "" {
			fmt.Fprintf(&b, " | intent:%s", truncIntent(e.Intent))
		}
		if !e.ExpiresAt.IsZero() {
			fmt.Fprintf(&b, " | ttl:%s", e.ExpiresAt.UTC().Format(time.RFC3339))
		}
		b.WriteByte('\n')
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
	}
	return nil
}

// EmitLLMStatusTargets writes a small positional table for per-file status.
func EmitLLMStatusTargets(w io.Writer, entries []StatusEntry) error {
	if _, err := io.WriteString(w, llmHeader); err != nil {
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
		intent := e.Intent
		if intent == "" {
			intent = "-"
		}
		if _, err := fmt.Fprintf(w, "✗ held | %s | %s | %s\n", e.Target, e.AgentID, intent); err != nil {
			return err
		}
	}
	return nil
}
