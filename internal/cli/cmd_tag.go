package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"loto/internal/domain"
	"loto/internal/store"
)

func init() { register("tag", cmdTag) } //nolint:gochecknoinits // command registry pattern

// tagUsage is the point-of-use teaching surface (loto-5rwc): Claude is loto's
// primary user, so the input contract lives in the binary, not a drift-prone
// skill. Convention: open the text with the requester's bead id, then a
// <=3-word ask. The bead id resolves epic/gh-issue via beads metadata — do not
// duplicate those here.
const tagUsage = `usage: loto tag <file> <text...>

Leave a note on a target locked by another agent.

Convention: open the text with your bead id, then a <=3-word ask.
The bead id resolves epic/gh-issue via beads metadata — don't duplicate them.

examples:
  loto tag internal/store/store.go loto-c6rg: want next
  loto tag a.go loto-5rwc: ETA?`

// beadIDPrefix matches a leading "<prefix>-<slug>:" bead reference, e.g.
// "loto-c6rg:". Used for light input shaping only — a miss WARNs, never rejects.
var beadIDPrefix = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]+:`)

func cmdTag(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, tagUsage) }
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, tagUsage)
		return 2
	}
	text := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if text == "" {
		fmt.Fprintln(stderr, "✗ tag text required")
		return 2
	}
	warnIfNoBeadID(text, stderr)
	repoTop, _ := repoTopForCwd(ctx)
	target, err := resolveCLITarget(repoTop, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 2
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	return runTag(rt, target.Canonical, text, stdout, stderr)
}

// warnIfNoBeadID shapes input lightly: if the tag text does not open with a
// "<bead-id>:" prefix, warn but proceed. Agents aren't always under a bead and
// humans tag too, so the free-text field stays — this is a nudge, not a gate.
func warnIfNoBeadID(text string, stderr io.Writer) {
	if beadIDPrefix.MatchString(text) {
		return
	}
	fmt.Fprintln(stderr, "∇ tag text should open with your bead id (e.g. loto-c6rg: want next)")
}

func runTag(rt *runtime, canonical, text string, stdout, stderr io.Writer) int {
	// Deliver to EVERY current holder, not an arbitrary one: a target held
	// shared by N agents has N blockers, and a note "on this file" must reach
	// each (loto-2nc5). InsertTag binds to the (target, owner, created_at)
	// triple, so one tag per holder.
	holders, err := rt.Store.LocksAt(rt.Ctx, domain.Target{Canonical: canonical})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(holders) == 0 {
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(canonical))
		return 3
	}
	var ids []string
	var firstErr error
	for i := range holders {
		id, err := rt.Store.InsertTag(rt.Ctx, store.NewTag{
			TargetCanonical: domain.Canonical(canonical),
			LockOwnerUUID:   string(holders[i].OwnerUUID),
			LockCreatedAt:   holders[i].CreatedAt.UnixNano(),
			TaggerUUID:      rt.Agent.UUID,
			Text:            text,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ids = append(ids, id)
	}
	// All holders rejected the note (every per-holder cap reached, or the lock
	// dropped mid-loop): surface the first error, nothing delivered.
	if len(ids) == 0 {
		return tagInsertErr(firstErr, canonical, stderr)
	}
	if len(ids) == 1 {
		fmt.Fprintf(stdout, "✓ tag id=%s target=%s\n", ids[0], relPath(canonical))
		return 0
	}
	fmt.Fprintf(stdout, "✓ tag delivered=%d target=%s ids=%s\n", len(ids), relPath(canonical), strings.Join(ids, ","))
	return 0
}

func tagInsertErr(err error, canonical string, stderr io.Writer) int {
	switch {
	case errors.Is(err, store.ErrTagCapReached):
		fmt.Fprintf(stderr, "✗ tag cap reached on %s (5) — escalate channel\n", relPath(canonical))
	case errors.Is(err, store.ErrNoHostLock):
		// Race: lock dropped between LockAt and InsertTag.
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(canonical))
	default:
		fmt.Fprintf(stderr, "✗ %v\n", err)
	}
	return 3
}
