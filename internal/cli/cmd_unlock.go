package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("unlock", cmdUnlock) } //nolint:gochecknoinits // command registry pattern

// holdRefList collects repeated --expect-holder values. Repeatable because a
// shared target has a SET of holders (I1) and a compare-and-swap over a set
// has to name every member — stating one of three and breaking all three would
// dispossess two agents the caller never looked at.
type holdRefList []domain.HoldRef

// errDuplicateExpectHolder is the static base for the repeated-token refusal;
// the token itself is wrapped in, per the repo's no-dynamic-errors rule.
var errDuplicateExpectHolder = errors.New("duplicate --expect-holder")

func (h *holdRefList) String() string { return domain.FormatHoldRefs(*h) }

// Set parses one `owner@epoch` token. A repeated token is rejected rather than
// deduped: the caller's statement of the holder set is wrong, and quietly
// fixing it up would let a typo pass a check whose whole job is exactness.
func (h *holdRefList) Set(v string) error {
	ref, err := domain.ParseHoldRef(v)
	if err != nil {
		return err
	}
	if slices.Contains(*h, ref) {
		return fmt.Errorf("%w: %s", errDuplicateExpectHolder, ref)
	}
	*h = append(*h, ref)
	return nil
}

// checkExpectHolderUsage refuses the three invocations where --expect-holder
// cannot mean what it says, before any store is opened.
//
// ‡ CAS is single-target on purpose. --expect-holder names holds, not targets,
// so across `unlock a.go b.go --force --expect-holder alice@3` there is no
// honest way to say which path alice@3 belongs to; binding it to all of them
// would silently exempt the others. A surgical break states one path; a sweep
// over dead territory is the blind form and always was. Multi-target CAS can
// come back as `path=owner@epoch` tokens if a caller ever needs it.
func checkExpectHolderUsage(expect holdRefList, force, all bool, nargs int, stderr io.Writer) int {
	if len(expect) == 0 {
		return 0
	}
	// --all takes the ReleaseBySession path, which reads no target and no
	// expectation: `--all --force --expect-holder alice@3 a.go` would sweep
	// every lock this uuid owns while reading as a guarded break of one path.
	// Refuse rather than let the sweep wear the CAS's clothes.
	if all {
		fmt.Fprintln(stderr, "✗ --expect-holder names one hold to break; --all releases your own locks and reads no target: drop one")
		return 2
	}
	if !force {
		fmt.Fprintln(stderr, "✗ --expect-holder is the compare-and-swap for --force; plain unlock only ever releases your own row")
		return 2
	}
	if nargs != 1 {
		fmt.Fprintf(stderr, "✗ --expect-holder takes exactly one target, got %d: state the hold for one path per call\n", nargs)
		return 2
	}
	return 0
}

func cmdUnlock(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "break a lock; BLIND unless --expect-holder names the hold to break")
	all := fs.Bool("all", false, "release every lock owned by my uuid")
	intent := fs.String("t", "", "intent (required only with --force)")
	fs.StringVar(intent, "intent", "", "intent (required only with --force)")
	var expect holdRefList
	fs.Var(&expect, "expect-holder", "owner@epoch (from loto status) this break must find; repeat per holder of a shared target; refuse if the holder set moved")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if code := checkExpectHolderUsage(expect, *force, *all, fs.NArg(), stderr); code != 0 {
		return code
	}
	// -t is required only for --force: BreakLocks records it in the break audit
	// trail. Plain unlock and --all discard intent (ReleaseLocks/ReleaseBySession
	// take no intent arg), so demanding it there was pure ceremony — and its
	// stderr+exit2 rejection read as a silent no-op to a stdout-only agent,
	// leaving locks dangling (loto-e0mz).
	if *force && *intent == "" {
		fmt.Fprintln(stderr, "✗ -t required to break a lock (--force): loto unlock <target> --force -t \"why\"")
		return 2
	}
	if !*all && fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loto unlock <target> [<target>...] [-t \"why\"] [--force [--expect-holder owner@epoch]] | --all -t \"why\"")
		return 2
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	if *all {
		return unlockAll(rt, stdout, stderr)
	}
	// Resolve repoTop so absolute paths inside the repo normalize to their
	// repo-relative canonical key, exactly like lock/check/status/tag. Without
	// this, `loto unlock /abs/path` hits ErrRepoEscape and the lock acquired by
	// the same absolute path is never released (loto-tel0).
	repoTop, _ := repoTopForCwd(ctx)
	if *force {
		return breakTargets(rt, fs.Args(), *intent, repoTop, expect, stdout, stderr)
	}
	return unlockTargets(rt, fs.Args(), repoTop, stdout, stderr)
}

// unlockTargets resolves CLI args to canonical targets and asks the store to
// release them in one batch, then renders per-target outcomes through the
// render package per docs/design.md.
func unlockTargets(rt *runtime, args []string, repoTop string, stdout, stderr io.Writer) int {
	targets, code := resolveUnlockArgs(args, repoTop, stderr)
	if code != 0 {
		return code
	}
	results, err := rt.Store.ReleaseLocks(rt.Ctx, targets, domain.AgentUUID(rt.Agent.UUID), rt.liveProbe())
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return render.EmitReleaseResults(stdout, results)
}

// breakTargets handles --force: single batched BreakLocks call. Per-target
// outcomes (success / no-lock / holder-changed / authorize-fail) come back in
// input order via BreakResult.Err so the render walks one slice instead of
// looping a single-target API.
//
// expect, when non-empty, is the compare-and-swap precondition. It attaches to
// the SOLE target — checkExpectHolderUsage has already refused a multi-target
// invocation — so the map this builds has exactly one key.
func breakTargets(rt *runtime, args []string, intent, repoTop string, expect holdRefList, stdout, stderr io.Writer) int {
	targets, code := resolveUnlockArgs(args, repoTop, stderr)
	if code != 0 {
		return code
	}
	var expectations store.BreakExpectations
	if len(expect) > 0 {
		expectations = store.BreakExpectations{targets[0].Canonical: expect}
	}
	// --force ORPHANS tags (gc-deletes them) rather than acking, by design
	// (edge #6, release-ack vs break-orphan). Force is the dead-holder escape
	// hatch, not a voluntary "you can go now" — the waiter retries at cycle-end.
	results, err := rt.Store.BreakLocks(rt.Ctx, targets, domain.AgentUUID(rt.Agent.UUID), store.BreakForce, intent, rt.liveProbe(), expectations)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return render.EmitBreakResults(stdout, stderr, results)
}

func resolveUnlockArgs(args []string, repoTop string, stderr io.Writer) ([]domain.Target, int) {
	out := make([]domain.Target, 0, len(args))
	for _, a := range args {
		t, err := resolveCLITarget(callerBase(), repoTop, a)
		if err != nil {
			fmt.Fprintf(stderr, "✗ target %q: %v\n", a, err)
			return nil, 2
		}
		out = append(out, t)
	}
	return out, 0
}

func unlockAll(rt *runtime, stdout, stderr io.Writer) int {
	// Guard: if no identity env var was set, Ensure minted a throwaway UUID
	// that owns zero locks. Reporting "0 released" and exiting 0 is a silent
	// false-success — the caller's real locks (held under a now-unreachable
	// UUID) remain in place with files write-stripped. Refuse instead.
	if !rt.AgentPinned {
		fmt.Fprintln(stderr, "✗ --all requires a pinned identity: set LOTO_AGENT_ID to the UUID shown by loto whoami in the session that holds the locks")
		return 2
	}

	// Scope: session-pinned → release only this session's locks (NORTH_STAR
	// invariant 5). Unpinned → agent-scoped fallback (empty sessionUUID
	// tells ReleaseBySession to match all sessions for this agent).
	//
	// ReleaseBySession is atomic: one SQL query finds+deletes matching rows
	// in a single tx, closing the TOCTOU gap where the old list+filter+release
	// dance could miss locks created between ListLocks and ReleaseLocks.
	var sessionFilter domain.SessionUUID
	if rt.SessionPinned {
		sessionFilter = rt.SessionUUID
	}
	// ReleaseBySession releases the agent's locks AND claims in one atomic tx
	// (same agent, session-if-pinned scope), so a session-end --all clears
	// claimed territory too — a crashed/ended agent's claim otherwise squats its
	// prefix until TTL, the reclamation-parity gap ei5 closes.
	results, claimPrefixes, err := rt.Store.ReleaseBySession(rt.Ctx, domain.AgentUUID(rt.Agent.UUID), sessionFilter)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	exit := render.EmitReleaseResults(stdout, results)
	render.EmitClaimsReleased(stdout, claimPrefixes)
	return exit
}
