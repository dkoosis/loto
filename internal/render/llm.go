package render

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const llmHeader = "loto:llm:v1\n"

// shortID returns the first 8 chars of a UUID-ish string for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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
