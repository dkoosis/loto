package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/gate"
	"loto/internal/render"
)

func init() { register("gate", cmdGate) } //nolint:gochecknoinits // command registry pattern

const gateUsageHead = `usage: loto gate stats [--since <dur>]

Report admission verdicts per rejection class over a window. Every judged
candidate leaves one audit row; this counts them.

Classes with zero counts are printed too — "which classes have never fired
here" is the question that decides whether the contamination story needs a
stronger mechanism than the gate.

The window is bounded by events retention (1000 rows / 7 days), so a --since
wider than that reports what the audit trail still holds.

examples:
  loto gate stats
  loto gate stats --since 168h
`

// gateStatsDefaultWindow is one day — long enough to cover a working session
// plus the night before it, short enough that "what is happening lately"
// isn't diluted by last week.
const gateStatsDefaultWindow = 24 * time.Hour

func cmdGate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "stats":
		return gateStats(ctx, args[1:], stdout, stderr)
	case "-h", flagHelpLong, subHelp:
		fmt.Fprint(stdout, gateUsageHead)
		return 0
	default:
		fmt.Fprint(stderr, gateUsageHead)
		return 2
	}
}

func gateStats(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.Duration("since", gateStatsDefaultWindow, "window to report over")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *since <= 0 {
		fmt.Fprintln(stderr, "✗ --since must be positive")
		return 2
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	st, err := rt.Store.ReadGateStats(rt.Ctx, *since)
	if err != nil {
		fmt.Fprintf(stderr, "✗ read gate stats: %v\n", err)
		return 3
	}
	// Taxonomy order, not map order: gate.RejectionReasons IS the report's
	// sort key, so the same window always renders byte-identically.
	classes := make([]render.GateStatsClass, len(gate.RejectionReasons))
	for i, r := range gate.RejectionReasons {
		classes[i] = render.GateStatsClass{Class: string(r), Count: st.ByClass[r]}
	}
	render.EmitGateStats(stdout, *since, st.Accepted, st.Rejected, st.Bypassed, classes)
	return 0
}
