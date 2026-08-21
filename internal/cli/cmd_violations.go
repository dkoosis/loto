package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("violations", cmdViolations) } //nolint:gochecknoinits // command registry pattern

const violationsUsageHead = `usage: loto violations [scan|resolve <id> [-m "<why>"]]

A violation is content in the working tree that differs from
refs/loto/integration on a path nothing authorized — no live lease, no live
candidate claim. It is STICKY: taking a lock on a contaminated path does not
clear it, which is what stops a rogue edit being laundered into integration
under a later, perfectly valid lease.

  (no args)   list unresolved violations
  scan        re-read the tree now; record what is new, auto-resolve reverts
  resolve     close one violation on the record

Reverting the change (git checkout refs/loto/integration -- <path>) resolves
it automatically on the next scan. Use resolve for a change that is
legitimate and staying.

examples:
  loto violations
  loto violations scan
  loto violations resolve v-1a2b3c4d -m "intentional vendored regen"
`

func cmdViolations(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		return violationsList(ctx, stdout, stderr)
	case "scan":
		return violationsScan(ctx, stdout, stderr)
	case "resolve":
		return violationsResolve(ctx, args[1:], stdout, stderr)
	case "-h", flagHelpLong, subHelp:
		fmt.Fprint(stdout, violationsUsageHead)
		return 0
	default:
		fmt.Fprint(stderr, violationsUsageHead)
		return 2
	}
}

func violationsList(ctx context.Context, stdout, stderr io.Writer) int {
	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	rows, code := violationRows(rt, stderr)
	if code != 0 {
		return code
	}
	render.EmitViolations(stdout, rows)
	if len(rows) > 0 {
		return 1
	}
	return 0
}

func violationsScan(ctx context.Context, stdout, stderr io.Writer) int {
	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	res, err := runViolationScan(rt, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ scan: %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "ℹ scan observed=%d recorded=%d resolved=%d\n",
		res.Observed, len(res.Recorded), res.Resolved)

	rows, code := violationRows(rt, stderr)
	if code != 0 {
		return code
	}
	render.EmitViolations(stdout, rows)
	if len(rows) > 0 {
		return 1
	}
	return 0
}

func violationsResolve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("violations resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	why := fs.String("m", "", "why this change is legitimate (recorded)")
	fs.StringVar(why, "message", "", "why this change is legitimate (recorded)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, `usage: loto violations resolve <id> [-m "<why>"]`)
		return 2
	}
	id := fs.Arg(0)

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if err := rt.Store.ResolveViolation(rt.Ctx, id, store.ResolutionAcked); err != nil {
		if errors.Is(err, store.ErrUnknownViolation) {
			fmt.Fprintf(stderr, "✗ no unresolved violation id=%s\n", id)
			return 3
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	fmt.Fprintf(stdout, "✓ violation-resolved id=%s reason=%s\n", id, resolveReason(*why))
	return 0
}

// resolveReason keeps the emitted row's shape fixed whether or not the
// operator supplied -m: a field that vanishes makes the output non-uniform to
// parse, which is the one thing every consumer of this stdout is doing.
func resolveReason(why string) string {
	if why == "" {
		return "acked"
	}
	return why
}

// runViolationScan is the one production producer of ReconcileScan's
// whole-tree input, and deliberately the only one: ReconcileScan auto-resolves
// every open row absent from the observation set, which is sound for a
// whole-tree pass and silently wrong for a partial one.
func runViolationScan(rt *runtime, repoTop string) (store.ScanResult, error) {
	obs, err := gate.ScanWorktree(rt.Ctx, repoTop)
	if err != nil {
		return store.ScanResult{}, err
	}
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}
	return rt.Store.ReconcileScan(rt.Ctx, obs, ec)
}

// violationRows reads the open set and lifts it into render shape.
func violationRows(rt *runtime, stderr io.Writer) ([]render.ViolationRow, int) {
	vs, err := rt.Store.UnresolvedViolations(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ read violations: %v\n", err)
		return nil, 3
	}
	rows := make([]render.ViolationRow, len(vs))
	for i, v := range vs {
		rows[i] = render.ViolationRow{
			ID: v.ID, Path: v.PathCanonical, ObservedAt: time.Unix(0, v.ObservedAt),
			LeaseState: v.LeaseState, Fingerprint: v.Fingerprint,
		}
	}
	return rows, 0
}

// violationNotice is the advisory other commands carry. Best-effort by
// contract: a store read that fails must not turn `loto status` or a
// PreToolUse check into an error, so the notice is simply omitted.
func violationNotice(rt *runtime, w io.Writer) {
	vs, err := rt.Store.UnresolvedViolations(rt.Ctx)
	if err != nil || len(vs) == 0 {
		return
	}
	rows := make([]render.ViolationRow, len(vs))
	for i, v := range vs {
		rows[i] = render.ViolationRow{
			ID: v.ID, Path: v.PathCanonical, ObservedAt: time.Unix(0, v.ObservedAt),
			LeaseState: v.LeaseState, Fingerprint: v.Fingerprint,
		}
	}
	render.EmitViolationNotice(w, rows)
}
