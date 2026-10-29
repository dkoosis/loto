package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
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
	onlyIntent := fs.String("only-intent", "", "with --all, release only locks whose recorded intent exactly matches this — scopes the sweep to one lane's own locks when a shared owner id also covers peer lanes (sd-xhap)")
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
		fmt.Fprintln(stderr, "usage: loto unlock <target> [<target>...] [-t \"why\"] [--force [--expect-holder owner@epoch]] | --all [--only-intent \"intent\"] -t \"why\"")
		return 2
	}
	onlyIntentSet := flagWasSet(fs, "only-intent")
	if onlyIntentSet && !*all {
		fmt.Fprintln(stderr, "✗ --only-intent scopes --all; it means nothing without it")
		return 2
	}
	// ‡ An empty value is refused rather than ignored (loto-lzap). Comparing
	// the parsed string against "" cannot tell `--only-intent ""` from an
	// absent flag, so a script whose variable expanded empty —
	// `loto unlock --all --only-intent "$INTENT"` — fell through to the
	// UNFILTERED sweep and released every lock and claim in scope. That is
	// sd-xhap's own incident shape, reached through the flag added to prevent
	// it. Passing the flag proves scoping was intended; an empty value means
	// the scope could not be determined, and a guard that cannot determine its
	// scope refuses instead of widening to everything.
	if onlyIntentSet && *onlyIntent == "" {
		fmt.Fprintln(stderr, "✗ --only-intent was given an empty value; it names the intent to release, and refusing beats sweeping every lock in scope")
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
		return unlockAll(rt, *onlyIntent, stdout, stderr)
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
	// The checkout the break acts in: the caller's own, unless the CAS names
	// a holder status shows in exactly one other checkout (loto-5dr1).
	st, worktree := rt.Store, rt.Worktree
	if len(expect) > 0 && len(targets) == 1 {
		wt, code := expectedHolderWorktree(rt, targets[0], expect, intent, stderr)
		if code != 0 {
			return code
		}
		if wt != "" {
			st, worktree = rt.Store.InWorktree(wt), wt
		}
	}
	// Snapshot every current holder BEFORE the break (loto-sblu). BreakLocks
	// deletes the lock rows — and gc-sweeps their tags — inside its own
	// transaction, so this is the only chance to learn who is about to be
	// dispossessed; asking again after the call would always find nobody.
	before := make(map[string][]domain.LockRecord, len(targets))
	for i := range targets {
		if holders, herr := rt.Store.LocksAt(rt.Ctx, targets[i]); herr == nil {
			// Same scope BreakLocks applies: a sibling worktree's row is not
			// broken, so it is not reported as dispossessed (loto-3eq6).
			before[targets[i].Canonical] = slices.DeleteFunc(holders, func(l domain.LockRecord) bool {
				return !domain.SameWorktree(worktree, l.Worktree)
			})
		}
	}
	var expectations store.BreakExpectations
	if len(expect) > 0 {
		// Restates locally what checkExpectHolderUsage already enforced
		// (--expect-holder ⇒ exactly one target), so the index is honest to
		// any reader — and to nilcheck, which cannot correlate
		// resolveUnlockArgs's nil-on-error return with its code!=0 companion.
		// Refuse rather than skip: dropping the expectation would turn a
		// guarded break into a blind one, the exact failure CAS exists to stop.
		if len(targets) != 1 {
			fmt.Fprintf(stderr, "✗ --expect-holder takes exactly one target, got %d\n", len(targets))
			return 2
		}
		expectations = store.BreakExpectations{targets[0].Canonical: expect}
	}
	// --force ORPHANS tags (gc-deletes them) rather than acking, by design
	// (edge #6, release-ack vs break-orphan). Force is the dead-holder escape
	// hatch, not a voluntary "you can go now" — the waiter retries at cycle-end.
	results, err := st.BreakLocks(rt.Ctx, targets, domain.AgentUUID(rt.Agent.UUID), store.BreakForce, intent, rt.liveProbe(), expectations)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	exit := render.EmitBreakResults(stdout, stderr, results)
	notifyDispossessed(rt, intent, time.Now(), before, results, stdout)
	return exit
}

// expectedHolderWorktree finds the checkout whose row on t the CAS names.
// `loto status` lists every checkout's rows and prints owner@epoch with no
// checkout, so a holder read there and stated back from the main checkout
// used to find nothing in the caller's own checkout: holder-changed,
// actual=none, for a lock status was showing (loto-5dr1).
//
// Returns "" to break in the caller's own checkout: the named holders sit
// only there, or nowhere (the CAS then reports holder-changed as before).
// Returns the one other checkout when every named holder sits there. Named
// holders spread over two or more checkouts cannot be one break: it
// refuses, exit 2, printing one runnable break per checkout.
func expectedHolderWorktree(rt *runtime, t domain.Target, expect holdRefList, intent string, stderr io.Writer) (string, int) {
	rows, err := rt.Store.LocksAt(rt.Ctx, t)
	if err != nil {
		return "", 0
	}
	checkouts := expectedHolderCheckouts(rows, expect, rt.Worktree)
	switch {
	case len(checkouts) == 0:
		return "", 0
	case len(checkouts) == 1 && checkouts[0] == rt.Worktree:
		return "", 0
	case len(checkouts) == 1:
		return checkouts[0], 0
	}
	fmt.Fprintf(stderr, "✗ expect-holder-split count=%d: the named holders sit in %d checkouts; break each from its own\n", len(expect), len(checkouts))
	fmt.Fprintln(stderr, "```bash")
	for _, line := range splitBreakLines(rows, checkouts, expect, t, intent, checkoutsByKey(rt.Ctx, rt.RepoTop, rt.Worktree)) {
		fmt.Fprintln(stderr, line)
	}
	fmt.Fprintln(stderr, "```")
	return "", 2
}

// expectedHolderCheckouts lists, sorted, the checkouts the named holders sit
// in, the caller's own (own) spelled as own. A row with no recorded checkout
// is in none of them in particular: every checkout's CAS counts it
// (domain.SameWorktree), so it decides nothing about where to break.
func expectedHolderCheckouts(rows []domain.LockRecord, expect holdRefList, own string) []string {
	var out []string
	for i := range rows {
		r := &rows[i]
		if r.Worktree == "" || !slices.Contains(expect, domain.HoldRef{Owner: r.OwnerUUID, Epoch: r.Epoch}) {
			continue
		}
		wt := r.Worktree
		if domain.SameWorktree(own, wt) {
			wt = own
		}
		out = append(out, wt)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// splitBreakLines is one break per checkout in others (WorktreeKeys, run
// from the path paths names for each), each naming the
// holders the caller named that sit there. Only those: a holder the caller
// never named is never added, so a row that appeared since the caller read
// `loto status` still refuses that line with holder-changed, the CAS's
// whole point.
//
// ‡ A named row with no recorded checkout goes on the FIRST line only. Every
// checkout's CAS counts it, and the first break deletes it with its own
// rows (deleteOwnedTx's filter matches an empty worktree), so a later line
// still naming it would find it gone and refuse.
func splitBreakLines(rows []domain.LockRecord, others []string, expect holdRefList, t domain.Target, intent string, paths map[string]string) []string {
	out := make([]string, 0, len(others))
	for n, wt := range others {
		var refs []domain.HoldRef
		for i := range rows {
			ref := domain.HoldRef{Owner: rows[i].OwnerUUID, Epoch: rows[i].Epoch}
			if !slices.Contains(expect, ref) || (rows[i].Worktree == "" && n > 0) {
				continue
			}
			if domain.SameWorktree(wt, rows[i].Worktree) {
				refs = append(refs, ref)
			}
		}
		domain.SortHoldRefs(refs)
		var b strings.Builder
		fmt.Fprintf(&b, "(cd %s && loto unlock %s --force -t %s", shellQuote(checkoutPath(paths, wt)), shellQuote(t.Canonical), shellQuote(intent))
		for _, ref := range refs {
			fmt.Fprintf(&b, " --expect-holder %s", shellQuote(ref.String()))
		}
		b.WriteString(")")
		out = append(out, b.String())
	}
	return out
}

// notifyDispossessed tells whoever a successful --force break just dispossessed
// (loto-sblu). A lock-scoped tag cannot survive the break — the host lock row
// it hangs off is gone the instant BreakLocks commits — so this pins a
// territory tag to the bare path instead, the same channel `loto tag` already
// uses to leave a note on ground nobody currently holds (D9, nug
// b2b0a9df507c: reuse the existing per-path channel, no new store).
//
// Best-effort: a write failure here never changes the break's own exit code
// (Rule 3) — it prints a ⚠ advisory row and moves on to the next target.
func notifyDispossessed(rt *runtime, intent string, brokenAt time.Time, before map[string][]domain.LockRecord, results []store.BreakResult, stdout io.Writer) {
	breaker := rt.Agent.UUID
	expires := brokenAt.Add(territoryTagTTL)
	text := fmt.Sprintf("unlock --force by %s at %s: %s",
		breaker, brokenAt.UTC().Format(time.RFC3339), intent)
	for i := range results {
		if results[i].Err != nil {
			continue // nothing was broken here — nothing to tell anyone
		}
		target := results[i].Target
		holders := before[target.Canonical]
		var dispossessed []string
		for j := range holders {
			if string(holders[j].OwnerUUID) != breaker {
				dispossessed = append(dispossessed, string(holders[j].OwnerUUID))
			}
		}
		if len(dispossessed) == 0 {
			continue
		}
		if _, err := rt.Store.InsertTerritoryTag(rt.Ctx, store.NewTerritoryTag{
			PathPrefix: target.Canonical,
			TaggerUUID: breaker,
			Text:       text,
			ExpiresAt:  expires.UnixNano(),
		}); err != nil {
			sort.Strings(dispossessed)
			fmt.Fprintf(stdout, "⚠ notify-failed target=%s holder=%s err=%v\n",
				relPath(target.Canonical), strings.Join(dispossessed, ","), err)
		}
	}
}

func resolveUnlockArgs(args []string, repoTop string, stderr io.Writer) ([]domain.Target, int) {
	out := make([]domain.Target, 0, len(args))
	cc := newCaseCache()
	base := callerBase()
	for _, a := range args {
		t, err := resolveCLITarget(cc, base, repoTop, a)
		if err != nil {
			fmt.Fprintf(stderr, "✗ target %q: %v\n", a, err)
			return nil, 2
		}
		out = append(out, t)
	}
	return out, 0
}

func unlockAll(rt *runtime, onlyIntent string, stdout, stderr io.Writer) int {
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
	var sessionFilter domain.SessionUUID
	if rt.SessionPinned {
		sessionFilter = rt.SessionUUID
	}

	// --only-intent scopes the sweep to one lane's own locks (sd-xhap): N
	// concurrent Claude Code lanes share one owner id (and, per loto-81n's own
	// fix, can share one session id too — sub-session identity is not carried
	// through), so agent/session scoping alone cannot tell one lane's locks
	// from a peer's mid-edit locks. Intent is the one field a lane controls
	// per call (`loto lock ... -t "<bead>: intent"`); matching it exactly lets
	// a lane release exactly what it took, in one command, without naming
	// targets one by one.
	//
	// ‡ Both the filter and the peer-intent warning ride INSIDE the release
	// transaction (loto-lzap). The first cut of this did the filtering in the
	// CLI — ListLocks, keep the matching intents, ReleaseLocks those targets —
	// and that dance loses the race it was written to win: insertOrRefreshLock
	// upserts `intent` on the composite PK (target_canonical, owner_uuid), so a
	// same-owner peer re-acquiring a target between the list and the delete
	// rewrites its intent, and ReleaseLocks, which classifies on target and
	// owner alone, then deletes the peer's lock. Same for the warning, which
	// could observe one intent from a pre-release ListLocks while the sweep
	// took two. One SELECT-and-DELETE under the op-flock answers both.
	//
	// Worktree is the caller's own checkout (its WorktreeKey): the one signal
	// that survives even when owner AND session ids both collapse onto one
	// value across sibling subagents sharing a parent Claude Code session's
	// env (loto-19bz). Not the branch — a branch switch in one worktree must
	// not make its own locks foreign (Codex #371).
	//
	// The one widening is Nested (loto-5dr1): a root session's sweep also
	// takes its rows from checkouts INSIDE its own — the ones Claude Code
	// makes for its Agent subagents with `isolation: worktree`. It needs a
	// pinned session (an owner-only sweep could take another session's rows)
	// and a shell that is not a subagent's. If that env signal is lost, an
	// ISOLATED sibling's sweep still cannot reach another's: sibling checkouts
	// are never nested inside one another (store.WorktreeScope).
	//
	// ‡ Residual, accepted with dk (PR for loto-5dr1): a subagent with no
	// checkout of its own stands in the root's checkout with the root's ids.
	// CLAUDE_CODE_CHILD_SESSION is the only signal that tells it from the
	// root; if a harness upgrade drops it, that subagent's --all also takes
	// its isolated siblings' nested rows.
	root := rt.SessionPinned && !identity.InSubagentShell()
	scope := store.WorktreeScope{Own: rt.Worktree}
	if root {
		scope.Nested = nestedCheckoutKeys(rt.Ctx, rt.RepoTop)
	}
	rel, err := rt.Store.ReleaseBySession(rt.Ctx, domain.AgentUUID(rt.Agent.UUID), sessionFilter, onlyIntent, scope)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(rel.Ambiguous) > 0 {
		fix := ambiguousFix{
			own:        reachedLocks(rt, sessionFilter, onlyIntent, scope),
			onlyIntent: onlyIntent,
			// Naming another checkout's rows as commands to run is safe only
			// where the caller cannot be a sibling: a root session standing in
			// the repo's main checkout, which no isolated subagent ever is.
			root:  root && rt.Worktree == store.WorktreeMain,
			paths: checkoutsByKey(rt.Ctx, rt.RepoTop, rt.Worktree),
		}
		return renderAmbiguousRefusal(stderr, rt.Worktree, rel.Ambiguous, fix)
	}

	// Warn when an UNFILTERED sweep actually spanned more than one intent —
	// the observable signal that a shared owner id was carrying more than this
	// lane's locks (sd-xhap AC2). Warn-and-report, not refuse: a genuine
	// end-of-session sweep across every lane is often exactly what is wanted,
	// and the caller who wants a surgical release has --only-intent. Printed
	// above the release rows so the count leads, per design.md.
	if onlyIntent == "" {
		warnPeerIntents(stdout, rel)
	}

	exit := render.EmitReleaseResults(stdout, rel.Results)
	// Claims ride along on the unfiltered sweep only: a session-end --all must
	// clear claimed territory too, or a crashed/ended agent's claim squats its
	// prefix until TTL (the reclamation-parity gap ei5 closes). A filtered
	// release returns none, so this is a no-op there.
	render.EmitClaimsReleased(stdout, rel.ClaimPrefixes)
	return exit
}

// warnPeerIntents names the distinct intents a full --all sweep just released,
// when there was more than one. It reads the store's report of the rows the
// transaction actually deleted — never a separate pre-release listing, which
// could miss the peer lock the sweep went on to take (loto-lzap).
func warnPeerIntents(stdout io.Writer, rel store.SessionRelease) {
	if len(rel.Intents) <= 1 {
		return
	}
	quoted := make([]string, len(rel.Intents))
	for i, in := range rel.Intents {
		quoted[i] = fmt.Sprintf("%q", in)
	}
	fmt.Fprintf(stdout, "⚠ unlock-all-peer-intents count=%d intents=%s\n", len(rel.Results), strings.Join(quoted, ","))
}

// ambiguousFix is what renderAmbiguousRefusal needs to print a fix that
// succeeds.
type ambiguousFix struct {
	own        map[string][]string // checkout key → canonicals of the rows the caller's scope reaches
	onlyIntent string
	root       bool              // the caller may be handed commands for the other checkouts' rows
	paths      map[string]string // checkout key → its path, to cd into (checkoutsByKey)
}

// renderAmbiguousRefusal reports a `--all` sweep ReleaseBySession refused
// outright (loto-19bz): at least one matching lock or claim was stamped from a
// checkout the caller's scope does not reach, so owner/session scoping alone
// could not tell it apart from a sibling subagent's row sharing this call's
// identity. Nothing was released — deterministic order per design.md, exit 1
// (advisory conflict), the holding checkout on every row.
//
// ‡ The fix block holds only commands that succeed (loto-5dr1: the old
// `--only-intent` hint was refused the same way whenever the other rows
// shared the intent). A caller that could be a sibling gets the release of
// its own checkout's rows and nothing that reaches another's; a root session
// in the main checkout also gets each other checkout's release, run from
// inside it, and then the sweep.
func renderAmbiguousRefusal(stderr io.Writer, worktree string, holds []store.AmbiguousHold, fix ambiguousFix) int {
	sort.Slice(holds, func(i, j int) bool {
		if holds[i].Kind != holds[j].Kind {
			return holds[i].Kind > holds[j].Kind // locks first
		}
		if holds[i].Worktree != holds[j].Worktree {
			return holds[i].Worktree < holds[j].Worktree
		}
		return holds[i].Canonical < holds[j].Canonical
	})
	fmt.Fprintf(stderr, "✗ identity ambiguous count=%d: rows held from a worktree other than %s — refusing --all rather than releasing another worktree's locks and claims\n", len(holds), worktree)
	for _, h := range holds {
		fmt.Fprintf(stderr, "  %s %s worktree=%s\n", h.Kind, relPath(h.Canonical), h.Worktree)
	}
	var lines []string
	if fix.root {
		fmt.Fprintln(stderr, "ℹ every row above is this session's, held from a checkout outside this one: release each from inside it, then sweep")
		lines = append(lines, otherWorktreeReleases(holds, fix.paths)...)
		sweep := "loto unlock --all"
		if fix.onlyIntent != "" {
			sweep += " --only-intent " + shellQuote(fix.onlyIntent)
		}
		lines = append(lines, sweep)
	} else {
		fmt.Fprintln(stderr, "ℹ the rows above may be a sibling subagent's: leave them; release your own rows by path")
		for _, wt := range slices.Sorted(maps.Keys(fix.own)) {
			lines = append(lines, cdUnlock(checkoutPath(fix.paths, wt), "unlock", fix.own[wt]))
		}
	}
	if len(lines) == 0 {
		fmt.Fprintln(stderr, "ℹ this checkout holds none of the rows; nothing here to release")
		return 1
	}
	fmt.Fprintln(stderr, "```bash")
	for _, l := range lines {
		fmt.Fprintln(stderr, l)
	}
	fmt.Fprintln(stderr, "```")
	return 1
}

// otherWorktreeReleases is the per-path release of every row in holds, run
// from inside the checkout that holds it, which is the scope a plain `loto
// unlock` acts in: one `unlock` per checkout, and one `unclaim` per claim,
// since unclaim takes exactly one prefix. holds is already sorted by kind,
// then worktree, then path.
func otherWorktreeReleases(holds []store.AmbiguousHold, paths map[string]string) []string {
	var out []string
	for i := 0; i < len(holds); {
		if holds[i].Kind == "claim" {
			out = append(out, cdUnlock(checkoutPath(paths, holds[i].Worktree), "unclaim", []string{holds[i].Canonical}))
			i++
			continue
		}
		j := i
		var canonicals []string
		for j < len(holds) && holds[j].Kind == holds[i].Kind && holds[j].Worktree == holds[i].Worktree {
			canonicals = append(canonicals, holds[j].Canonical)
			j++
		}
		out = append(out, cdUnlock(checkoutPath(paths, holds[i].Worktree), "unlock", canonicals))
		i = j
	}
	return out
}

// cdUnlock renders `(cd '<checkout>' && loto <verb> '<path>'...)`. Canonicals
// are repo-relative, so they resolve from the checkout's toplevel whatever
// the caller's own cwd is.
func cdUnlock(checkout, verb string, paths []string) string {
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = shellQuote(p)
	}
	return fmt.Sprintf("(cd %s && loto %s %s)", shellQuote(checkout), verb, strings.Join(quoted, " "))
}

// reachedLocks groups, by checkout, the canonicals of the rows a refused
// sweep would have released: the caller's own checkout's (keyed by its
// key), and each nested subagent checkout's when scope reaches it.
// Read outside the refused transaction: it only feeds a printed fix, and a
// plain `loto unlock` re-decides each path under its own lock anyway.
func reachedLocks(rt *runtime, session domain.SessionUUID, onlyIntent string, scope store.WorktreeScope) map[string][]string {
	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for i := range locks {
		l := &locks[i]
		if string(l.OwnerUUID) != rt.Agent.UUID || (session != "" && l.SessionUUID != session) ||
			(onlyIntent != "" && l.Intent != onlyIntent) || !scope.Reaches(l.Worktree) {
			continue
		}
		wt := l.Worktree
		if domain.SameWorktree(rt.Worktree, wt) {
			wt = rt.Worktree
		}
		out[wt] = append(out[wt], l.Target.Canonical)
	}
	for wt, paths := range out {
		slices.Sort(paths)
		out[wt] = slices.Compact(paths)
	}
	return out
}
