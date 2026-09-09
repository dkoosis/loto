package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"loto/internal/render"
)

func init() { register("events", cmdEvents) } //nolint:gochecknoinits // command registry pattern

const eventsUsageHead = `usage: loto events [--kind <kind>] [--limit <n>]

Print the store's audit rows, oldest first. The read side of every counter
loto writes — lock acquire/release/break, admission verdicts, the staged-lock
gate's firings, and the tree hook's per-call timing.

The window is bounded by events retention (1000 rows / 7 days), so this shows
what the trail still holds, not everything that ever happened.

examples:
  loto events
  loto events --kind hook_timing
  loto events --kind hook_timing --limit 20
`

// eventsDefaultLimit caps the default read. The retention bound is 1000 rows,
// and dumping all of them into an agent's context to answer "did the hook fire"
// is the output-hygiene failure this repo's design rules name outright.
const eventsDefaultLimit = 50

func cmdEvents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, eventsUsageHead)
		fs.PrintDefaults()
	}
	kind := fs.String("kind", "", "show only this event kind")
	limit := fs.Int("limit", eventsDefaultLimit, "most recent rows to print; 0 for every retained row")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "✗ --limit must not be negative")
		return 2
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	evs, err := rt.Store.ListEvents(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ read events: %v\n", err)
		return 3
	}
	rows := make([]render.EventRow, 0, len(evs))
	for i := range evs {
		if *kind != "" && evs[i].Kind != *kind {
			continue
		}
		rows = append(rows, render.EventRow{
			CreatedAt: evs[i].CreatedAt,
			Kind:      evs[i].Kind,
			ActorUUID: evs[i].ActorUUID,
			Target:    evs[i].Target.Canonical,
			Reason:    evs[i].Reason,
			Detail:    evs[i].Detail,
		})
	}
	total := len(rows)
	// Trim from the FRONT: the newest rows are the ones a caller asking "did
	// it fire" needs, and ListEvents returns oldest first.
	if *limit > 0 && len(rows) > *limit {
		rows = rows[len(rows)-*limit:]
	}
	render.EmitEvents(stdout, rows, *kind, total)
	return 0
}
