package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
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
	return unlockTargets(rt, fs.Args(), *intent, *force, stdout, stderr)
}

func unlockTargets(rt *runtime, args []string, intent string, force bool, stdout, stderr io.Writer) int {
	live := func(host string, pid int) bool {
		if host != rt.Host {
			return true
		}
		return pidLive(pid)
	}
	code := 0
	for _, arg := range args {
		targets, resolveErr := resolveTargets(arg)
		if resolveErr != nil {
			fmt.Fprintf(stderr, "✗ target %q: %v\n", arg, resolveErr)
			code = 2
			continue
		}
		for _, target := range targets {
			if c := releaseOne(rt, target, intent, force, live, stdout, stderr); c != 0 {
				code = c
			}
		}
	}
	return code
}

func releaseOne(rt *runtime, target domain.Target, intent string, force bool, live func(string, int) bool, stdout, stderr io.Writer) int {
	if force {
		err := rt.Store.BreakLock(rt.Ctx, target, rt.Agent.UUID, true, intent, live)
		if err != nil {
			if errors.Is(err, store.ErrNoLockAtTarget) {
				fmt.Fprintf(stderr, "✗ no lock at target=%s\n", target.Canonical)
				return 1
			}
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 3
		}
		fmt.Fprintf(stdout, "✓ broken target=%s\n", target.Canonical)
		return 0
	}

	results, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{target}, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(results) == 0 {
		fmt.Fprintf(stderr, "✗ no result for target=%s\n", target.Canonical)
		return 3
	}
	switch r := results[0]; r.State {
	case store.StateUnlocked:
		fmt.Fprintf(stdout, "✓ unlocked target=%s\n", target.Canonical)
		return 0
	case store.StateNoLock:
		fmt.Fprintf(stderr, "✗ no lock at target=%s\n", target.Canonical)
		return 1
	case store.StateNotOwner:
		if err2 := rt.Store.BreakLock(rt.Ctx, target, rt.Agent.UUID, false, intent, live); err2 != nil {
			fmt.Fprintf(stderr, "✗ not owner and lock is live — use --force to override\n")
			return 1
		}
		fmt.Fprintf(stdout, "✓ reclaimed target=%s\n", target.Canonical)
		return 0
	case store.StateRestoreFailed:
		fmt.Fprintf(stderr, "✗ unlocked but mode-restore failed target=%s err=%v\n", relPath(target.Canonical), r.RestoreErr)
		return 1
	default:
		fmt.Fprintf(stderr, "✗ unexpected release state=%d target=%s\n", r.State, target.Canonical)
		return 3
	}
}

func unlockAll(rt *runtime, intent string, stdout, stderr io.Writer) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
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
	results, err := rt.Store.ReleaseLocks(rt.Ctx, mine, rt.Agent.UUID)
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
