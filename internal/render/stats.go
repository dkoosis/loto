package render

import (
	"fmt"
	"io"
	"time"
)

// GateStatsClass is one taxonomy class's count. Classes at zero are included
// by the caller on purpose — see EmitGateStats.
type GateStatsClass struct {
	Class string
	Count int
}

// EmitGateStats renders `loto gate stats`: triage counts on the first body
// line, then one row per taxonomy class in the caller's order.
//
// ‡ Zero-count classes print as ℹ rather than being dropped. A report that
// shows only what fired teaches nothing about what never does — and "which
// rejection classes have never once fired here" is exactly the question that
// decides whether PolicyFS reopens (git-gate.md: contamination classes are
// the only reopen trigger).
func EmitGateStats(w io.Writer, since time.Duration, accepted, rejected, bypassed int, classes []GateStatsClass) {
	glyph := "✓"
	if rejected > 0 || bypassed > 0 {
		glyph = "✗"
	}
	fmt.Fprintf(w, "%s gate-stats since=%s judged=%d accepted=%d rejected=%d bypassed=%d\n",
		glyph, since, accepted+rejected, accepted, rejected, bypassed)
	for _, c := range classes {
		rowGlyph := "ℹ"
		if c.Count > 0 {
			rowGlyph = "✗"
		}
		fmt.Fprintf(w, "%s class=%s count=%d\n", rowGlyph, c.Class, c.Count)
	}
}
