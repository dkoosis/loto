package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"loto/internal/render"
	"loto/internal/store"
)

const statsUsageHead = `usage: loto stats [--since <dur>]

Print enforcement-design.md §10b's four build-order counters: is I1 refusing
the right things, is drift detection reporting anything a holder acts on,
what the tree hook costs, and how often a post-hook goes missing. Each row
carries the number and the threshold it is judged against — the numbers
these thresholds settle, not memory.

--since accepts a Go duration (30m, 24h) or a day count (30d, 7d).

examples:
  loto stats
  loto stats --since 30d
  loto stats --since 7d
`

// statsDefaultWindow matches gate stats' default: long enough to cover a
// working session plus the night before it.
const statsDefaultWindow = 24 * time.Hour

func init() { register("stats", cmdStats) } //nolint:gochecknoinits // command registry pattern

func cmdStats(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", flagHelpLong, subHelp:
			fmt.Fprint(stdout, statsUsageHead)
			return 0
		}
	}

	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := statsDefaultWindow
	fs.Var(sinceFlag{&since}, "since", "window to report over (Go duration, or a day count like 30d)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if since <= 0 {
		fmt.Fprintln(stderr, "✗ --since must be positive")
		return 2
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	st, err := rt.Store.ReadEnforcementStats(rt.Ctx, since)
	if err != nil {
		fmt.Fprintf(stderr, "✗ read enforcement stats: %v\n", err)
		return 3
	}
	render.EmitEnforcementStats(stdout, enforcementStatsToRender(st))
	return 0
}

// enforcementStatsToRender converts the store's raw counters to render's own
// input shape — the same decoupling ReadGateStats/EmitGateStats use, so
// render never imports store.
func enforcementStatsToRender(st store.EnforcementStats) render.EnforcementStats {
	top := make([]render.HookTimingTop, len(st.TopLockedBytes))
	for i, t := range st.TopLockedBytes {
		top[i] = render.HookTimingTop{CallID: t.CallID, LockedBytes: t.LockedBytes}
	}
	return render.EnforcementStats{
		Since:                st.Since,
		RefRefused:           st.RefRefused,
		RefRefusedOverridden: st.RefRefusedOverridden,
		TreeChangeReported:   st.TreeChangeReported,
		TreeChangeActed:      st.TreeChangeActed,
		HookCallP50:          st.HookCallP50,
		HookCallP99:          st.HookCallP99,
		HookCalls:            st.HookCalls,
		TopLockedBytes:       top,
		PostMissing:          st.PostMissing,
		PostMissingResolved:  st.PostMissingResolved,
		ResolvedMeanAge:      st.ResolvedMeanAge,
	}
}

// sinceFlag is a flag.Value wrapping a *time.Duration, extending
// time.ParseDuration with a plain day count ("30d", "7d") — the unit §10b's
// own examples use and time.ParseDuration does not support (it stops at
// hours). Tried first as a standard Go duration; "<n>d" only on that failure,
// so "90m" and "36h" keep meaning what they always meant.
type sinceFlag struct{ d *time.Duration }

func (f sinceFlag) String() string {
	if f.d == nil {
		return ""
	}
	return f.d.String()
}

// errInvalidSinceDuration is sinceFlag.Set's one failure, wrapped with the
// rejected input rather than built fresh each call (err113: static errors,
// dynamic wrapping).
var errInvalidSinceDuration = errors.New("invalid duration (want a Go duration like 24h, or a day count like 30d)")

func (f sinceFlag) Set(s string) error {
	if d, err := time.ParseDuration(s); err == nil {
		*f.d = d
		return nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err == nil && n > 0 {
			*f.d = time.Duration(n * 24 * float64(time.Hour))
			return nil
		}
	}
	return fmt.Errorf("%w: %q", errInvalidSinceDuration, s)
}
