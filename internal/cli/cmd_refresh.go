package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("refresh", cmdRefresh) } //nolint:gochecknoinits // command registry pattern

// refreshUsageHead is the point-of-use teaching surface for refresh (loto-5rwc):
// usage line plus worked examples. The flag list is appended by PrintDefaults.
const refreshUsageHead = `usage: loto refresh <target> [<target>...] [--ttl 30m] | --all [--ttl 30m]

Extend the TTL on locks you already hold, in place — the "I'm still alive"
heartbeat. A lock whose TTL lapses is reclaimable by any peer's next acquire
with no doctor run, so a long job must refresh before its deadline. Refreshing
an already-expired lease is refused: re-acquire instead.

examples:
  loto refresh internal/store/store.go --ttl 1h
  loto refresh --all
`

func cmdRefresh(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, refreshUsageHead)
		fs.PrintDefaults()
	}
	ttl := fs.Duration("ttl", domain.DegradedModeTTL, "new lock TTL, measured from now")
	all := fs.Bool("all", false, "refresh every lock owned by my uuid")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	// Parity with lock/claim: a non-positive TTL mints an instantly-reclaimable
	// lease, which is never what a heartbeat means.
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be positive, got %s\n", *ttl)
		return 2
	}
	if !*all && fs.NArg() == 0 {
		fmt.Fprint(stderr, refreshUsageHead)
		fs.PrintDefaults()
		return 2
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	targets, code := refreshTargets(rt, fs.Args(), *all, stderr)
	if code != 0 {
		return code
	}
	if len(targets) == 0 {
		fmt.Fprint(stdout, "ℹ no locks owned\n")
		return 0
	}

	results, err := rt.Store.RefreshLocks(rt.Ctx, targets, domain.AgentUUID(rt.Agent.UUID), *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	// --all named no target, so a lease that lapsed before the heartbeat ran is
	// reported but must not fail the run (see emitRefreshResults).
	return emitRefreshResults(stdout, results, *all)
}

// refreshTargets resolves the target set: --all reads back every lock this
// agent currently holds, otherwise the CLI args are canonicalized the same way
// lock/downgrade do. --all requires a pinned identity for the same reason
// `unlock --all` does — an unpinned throwaway UUID owns zero locks, so a
// cheerful "0 refreshed" would be a silent false-success while the real locks
// drift toward expiry.
func refreshTargets(rt *runtime, args []string, all bool, stderr io.Writer) ([]domain.Target, int) {
	if !all {
		targets, invalid := validateLockTargets(args, rt.RepoTop, false)
		if len(invalid) > 0 {
			render.EmitInvalid(stderr, invalid)
			return nil, 2
		}
		return targets, 0
	}
	if !rt.AgentPinned {
		fmt.Fprintln(stderr, "✗ --all requires a pinned identity: set LOTO_AGENT_ID to the UUID shown by loto whoami in the session that holds the locks")
		return nil, 2
	}
	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return nil, 3
	}
	var targets []domain.Target
	for i := range locks {
		if string(locks[i].OwnerUUID) == rt.Agent.UUID {
			targets = append(targets, locks[i].Target)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Canonical < targets[j].Canonical })
	return targets, 0
}

// emitRefreshResults renders the count-first triage line then one row per
// target (css:cli convention, mirroring EmitReleaseResults). Exit 1 when any
// target failed — a partial refresh must not read as success to a caller that
// only checks the exit code.
//
// allTargets flips a lapsed lease from ✗/exit-1 to ⚠/exit-0: under --all the
// target set is whatever this agent happens to own, so one lock that lapsed
// before the heartbeat ran would otherwise poison the exit code of a run whose
// live leases were all extended. An explicitly named target keeps ✗ and exit 1
// — there the caller asked about that lock and the answer is "you lost it".
func emitRefreshResults(stdout io.Writer, results []store.RefreshResult, allTargets bool) int {
	ok := 0
	for i := range results {
		if results[i].Err == nil {
			ok++
		}
	}
	fmt.Fprintf(stdout, "✓ refreshed count=%d\n", ok)
	exit := 0
	for i := range results {
		r := &results[i]
		path := relPath(r.Target.Canonical)
		switch {
		case r.Err == nil:
			fmt.Fprintf(stdout, "✓ target=%s expires=%s\n", path, r.ExpiresAt.Format(time.RFC3339))
		case errors.Is(r.Err, store.ErrNoLockAtTarget):
			fmt.Fprintf(stdout, "✗ target=%s reason=no-lock-held\n", path)
			exit = 1
		case errors.Is(r.Err, store.ErrLeaseExpired):
			if allTargets {
				fmt.Fprintf(stdout, "⚠ target=%s reason=lease-expired hint=re-acquire\n", path)
				continue
			}
			fmt.Fprintf(stdout, "✗ target=%s reason=lease-expired hint=re-acquire\n", path)
			exit = 1
		default:
			fmt.Fprintf(stdout, "✗ target=%s reason=%v\n", path, r.Err)
			exit = 1
		}
	}
	return exit
}
