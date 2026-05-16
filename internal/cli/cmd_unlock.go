package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("unlock", cmdUnlock) } //nolint:gochecknoinits // command registry pattern

func cmdUnlock(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "break another agent's lock (or a live lock)")
	all := fs.Bool("all", false, "release every lock owned by my uuid")
	intent := fs.String("t", "", "intent (required)")
	fs.StringVar(intent, "intent", "", "intent (required)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *intent == "" {
		fmt.Fprintln(stderr, "✗ -t required: loto unlock <target> [<target>...] -t \"why\"")
		return 2
	}
	if !*all && fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loto unlock <target> [<target>...] [-t \"why\"] [--force] | --all -t \"why\"")
		return 2
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if *all {
		return unlockAll(rt, *intent, stdout, stderr)
	}
	if *force {
		return breakTargets(rt, fs.Args(), *intent, stdout, stderr)
	}
	return unlockTargets(rt, fs.Args(), stdout, stderr)
}

// unlockTargets resolves CLI args to canonical targets and asks the store to
// release them in one batch, then renders per-target outcomes through the
// render package per docs/design.md.
func unlockTargets(rt *runtime, args []string, stdout, stderr io.Writer) int {
	targets, code := resolveUnlockArgs(args, stderr)
	if code != 0 {
		return code
	}
	results, err := rt.Locks().ReleaseLocks(rt.Ctx, targets, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return render.EmitReleaseResults(stdout, results)
}

// breakTargets handles --force: single batched BreakLocks call. Per-target
// outcomes (success / no-lock / authorize-fail) come back in input order via
// BreakResult.Err so the render walks one slice instead of looping a single-
// target API.
func breakTargets(rt *runtime, args []string, intent string, stdout, stderr io.Writer) int {
	targets, code := resolveUnlockArgs(args, stderr)
	if code != 0 {
		return code
	}
	live := func(host string, pid int) bool {
		if host != rt.Host {
			return true
		}
		return pidLive(pid)
	}
	results, err := rt.Locks().BreakLocks(rt.Ctx, targets, rt.Agent.UUID, store.BreakForce, intent, live)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	exit := 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			fmt.Fprintf(stdout, "✓ broken target=%s\n", relPath(r.Target.Canonical))
		case errors.Is(r.Err, store.ErrNoLockAtTarget):
			fmt.Fprintf(stderr, "✗ no lock at target=%s\n", relPath(r.Target.Canonical))
			if exit < 1 {
				exit = 1
			}
		default:
			fmt.Fprintf(stderr, "✗ target=%s err=%v\n", relPath(r.Target.Canonical), r.Err)
			exit = 3
		}
	}
	return exit
}

func resolveUnlockArgs(args []string, stderr io.Writer) ([]domain.Target, int) {
	out := make([]domain.Target, 0, len(args))
	for _, a := range args {
		ts, err := resolveTargets(a)
		if err != nil {
			fmt.Fprintf(stderr, "✗ target %q: %v\n", a, err)
			return nil, 2
		}
		out = append(out, ts...)
	}
	return out, 0
}

func unlockAll(rt *runtime, intent string, stdout, stderr io.Writer) int {
	all, err := rt.Locks().ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	// Scope: agent always, session iff LOTO_SESSION_ID is pinned. Pinning is
	// the SessionStart hook signaling "I am one Claude session of many" — in
	// that case --all must not release sibling sessions' holdings (NORTH_STAR
	// invariant 5). Without pinning (interactive single-shot use), fall back
	// to agent-scoped — otherwise --all matches nothing and silently fails.
	mine := make([]domain.Target, 0, len(all))
	for i := range all {
		if all[i].OwnerUUID != rt.Agent.UUID {
			continue
		}
		if rt.SessionPinned && all[i].SessionUUID != rt.SessionUUID {
			continue
		}
		mine = append(mine, all[i].Target)
	}
	results, err := rt.Locks().ReleaseLocks(rt.Ctx, mine, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	n := 0
	for _, r := range results {
		if r.State == store.StateUnlocked {
			n++
		}
	}
	fmt.Fprintf(stdout, "✓ released count=%d intent=%q\n", n, intent)
	return 0
}
