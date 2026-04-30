package render

import (
	"fmt"
	"io"
	"time"
)

// DashboardEvent is the render-side projection of loto.Event. The render
// package intentionally avoids importing the loto root, so callers translate.
type DashboardEvent struct {
	Time   time.Time
	Kind   string // "held" | "released" | "reserved" | "unreserved" | "msg"
	Agent  string
	Target string
	Intent string
	To     string
	Body   string
}

// EmitLLMDashboardEvent writes a single event line in LLM format.
// Caller is responsible for the loto:llm:v1 header (the dashboard streams,
// so the header is emitted once at start by EmitLLMDashboardHeader).
func EmitLLMDashboardEvent(w io.Writer, e DashboardEvent) error {
	ts := rfc3339UTC(e.Time)
	switch e.Kind {
	case "msg":
		_, err := fmt.Fprintf(w, "→ ts:%s | msg | from:%s | to:%s | target:%s | %s\n",
			ts, e.Agent, e.To, orDash(RelPath(e.Target)), collapseBody(e.Body))
		return err
	case "held", "released":
		_, err := fmt.Fprintf(w, "→ ts:%s | %s | agent:%s | target:%s | intent:%s\n",
			ts, e.Kind, e.Agent, RelPath(e.Target), truncIntent(e.Intent))
		return err
	case "reserved", "unreserved":
		_, err := fmt.Fprintf(w, "→ ts:%s | %s | agent:%s | pattern:%s | intent:%s\n",
			ts, e.Kind, e.Agent, e.Target, truncIntent(e.Intent))
		return err
	}
	return nil
}

// EmitLLMDashboardHeader writes the version sentinel. Streams call this once
// before emitting events.
func EmitLLMDashboardHeader(w io.Writer) error {
	return writeHeader(w)
}

// EmitHumanDashboardEvent writes a human-friendly one-line form for tty.
// Layout: "HH:MM:SS  Agent  kind  target  intent: ..."
func EmitHumanDashboardEvent(w io.Writer, e DashboardEvent) error {
	hm := e.Time.Local().Format("15:04:05")
	switch e.Kind {
	case "msg":
		_, err := fmt.Fprintf(w, "%s  %s → %s: %s\n", hm, e.Agent, e.To, collapseBody(e.Body))
		return err
	case "held":
		_, err := fmt.Fprintf(w, "%s  %s held    %s%s\n", hm, e.Agent, RelPath(e.Target), humanIntent(e.Intent))
		return err
	case "released":
		_, err := fmt.Fprintf(w, "%s  %s released %s\n", hm, e.Agent, RelPath(e.Target))
		return err
	case "reserved":
		_, err := fmt.Fprintf(w, "%s  %s reserved %s%s\n", hm, e.Agent, e.Target, humanIntent(e.Intent))
		return err
	case "unreserved":
		_, err := fmt.Fprintf(w, "%s  %s released-reservation %s\n", hm, e.Agent, e.Target)
		return err
	}
	return nil
}

func humanIntent(s string) string {
	if s == "" {
		return ""
	}
	return "  intent: " + truncIntent(s)
}
