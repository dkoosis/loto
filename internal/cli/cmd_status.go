package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
)

func init() { register("status", cmdStatus) } //nolint:gochecknoinits // command registry pattern

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mine := fs.Bool("mine", false, "show only locks and claims owned by my uuid")
	collisions := fs.Bool("collisions", false, "report only targets held live by ≥2 distinct owners (shared-beacon collisions)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	repoTop, _ := repoTopForCwd(ctx)
	fmt.Fprintf(stdout, "project: %s\n", ResolveAndPinProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", rt.StateDir)
	fmt.Fprintf(stdout, "guard:   %s\n", guardSummary(checkGuardReachability(ctx, repoTop)))

	if *collisions {
		return statusCollisions(stdout, stderr, rt)
	}

	if fs.NArg() == 1 {
		t, err := resolveCLITarget(nil, callerBase(), repoTop, fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 2
		}
		return statusSingleTarget(stdout, rt, t)
	}

	return statusWholeRepo(stdout, stderr, rt, *mine)
}

// statusWholeRepo renders the no-target report: locks, claims, territory
// tags, then any unresolved violations.
func statusWholeRepo(stdout, stderr io.Writer, rt *runtime, mine bool) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if mine {
		all = filterLocksByOwner(all, rt.Agent.UUID)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Target.Canonical != all[j].Target.Canonical {
			return all[i].Target.Canonical < all[j].Target.Canonical
		}
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})
	locksForeign := printStatusLocks(stdout, rt, all, mine)

	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	now := time.Now()
	claimsForeign := printStatusClaims(stdout, rt, claims, mine, now)
	// The fix block sits right after the two sections it can apply to
	// (ferret-m3t): a row belonging to someone else is only ever a lock or a
	// claim row (territory tags carry no self marker — see
	// printStatusTerritoryTags), and `mine` already means every remaining row
	// is the caller's own, so *Foreign is always false there — no gate needed.
	if locksForeign || claimsForeign {
		emitForeignRowFixBlock(stdout)
	}
	printStatusTerritoryTags(stdout, rt, mine, now)
	// Read-only resurfacing: status reports what the store already knows, it
	// does not run the sensor. Recording is a whole-tree git diff, and making
	// every `loto status` pay for one would put a scan on the path of the
	// command agents run most often to orient. `loto violations scan` and
	// `loto submit` are the two producers.
	violationNotice(rt, stdout)
	return 0
}

// printStatusTerritoryTags renders every live note pinned to this repo's
// territory (loto-z3y1). This section is what keeps anyone from wanting an
// inbox verb: "what is pinned on this ground" already has a door, and it is
// the same door that answers "who holds what".
//
// ‡ Suppressed at zero, unlike the locks and claims sections. Those print an
// explicit empty header because silence there would read as a crash — status
// is ABOUT holdings. A repo with no notes is the overwhelmingly common case,
// and a permanent "✓ no territory-tags" line would tax every status call to
// report the absence of a feature most repos never use.
//
// --mine filters to notes this agent wrote, matching the flag's meaning in the
// sections above. Self-authored notes are NOT excluded here: status answers
// "what is pinned", where your own note is part of the answer, unlike the
// footer which answers "what should I know".
func printStatusTerritoryTags(stdout io.Writer, rt *runtime, mine bool, now time.Time) {
	notes := liveTerritoryTags(rt, now)
	if mine {
		kept := notes[:0]
		for _, n := range notes {
			if n.TaggerUUID == rt.Agent.UUID {
				kept = append(kept, n)
			}
		}
		notes = kept
	}
	sortTerritoryTags(notes)
	render.EmitTerritoryTagRows(stdout, notes, "territory-tags", "")
}

// printStatusClaims renders the claims section after locks (loto-7af9): live
// rows only — Expired is display-time authority; the row itself dies lazily in
// a later overlapping acquire — sorted prefix then created_at, --mine honored.
// Explicit empty header per design.md: silence looks like a crash.
// printStatusClaims returns foreign — whether any printed row belongs to
// someone other than the caller (ferret-m3t) — on the same terms as
// printStatusLocks: self=true marks the caller's own rows, suppressed under
// --mine because every remaining row is already its own there and marking
// them would move the golden output the AC pins unchanged.
func printStatusClaims(stdout io.Writer, rt *runtime, all []domain.ClaimRecord, mine bool, now time.Time) (foreign bool) {
	live := all[:0]
	for i := range all {
		if all[i].Expired(now) {
			continue
		}
		if mine && string(all[i].OwnerUUID) != rt.Agent.UUID {
			continue
		}
		live = append(live, all[i])
	}
	if len(live) == 0 {
		fmt.Fprintln(stdout, "✓ no claims")
		return false
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].PathPrefix != live[j].PathPrefix {
			return live[i].PathPrefix < live[j].PathPrefix
		}
		return live[i].CreatedAt.Before(live[j].CreatedAt)
	})
	fmt.Fprintf(stdout, "✓ claims count=%d\n", len(live))
	for i := range live {
		c := &live[i]
		self := ""
		if string(c.OwnerUUID) == rt.Agent.UUID {
			if !mine {
				self = " self=true"
			}
		} else {
			foreign = true
		}
		fmt.Fprintf(stdout, "✓ prefix=%s owner=%s intent=%q held_since=%s ttl_remaining=%s host=%s%s\n",
			relPath(c.PathPrefix), c.OwnerUUID, c.Intent,
			c.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(c.ExpiresAt.Sub(now)), c.Host, self)
	}
	return foreign
}

// emitForeignRowFixBlock is the inline fix block design.md §9 requires under
// an actionable finding (ferret-m3t): the unfiltered dump containing a row
// the caller does not hold is exactly that finding — a raw owner=<uuid> the
// reader cannot itself attribute — and status shipped no fix for it before
// this. Names the verbs that resolve it: whoami confirms the caller's own
// uuid (the same one self=true above is checked against), -mine narrows the
// view to only what the caller holds, and tag reaches a peer holding a target
// directly rather than guessing from timestamps — the guess that cost two
// lanes the night this bead was filed.
func emitForeignRowFixBlock(stdout io.Writer) {
	fmt.Fprintln(stdout, "‡ rows above with no self=true belong to a peer — attribute before acting on them:")
	fmt.Fprintln(stdout, "```bash")
	fmt.Fprintln(stdout, "loto whoami                    # confirm your own uuid")
	fmt.Fprintln(stdout, "loto status -mine               # narrow to only what you hold")
	fmt.Fprintln(stdout, "loto tag <path> \"<bead>: ask\"   # reach the peer holding a target you need")
	fmt.Fprintln(stdout, "```")
}

// statusCollisions reports every target held live by ≥2 DISTINCT owners — the
// shared-beacon collision the gate deliberately can't see. `loto check` treats
// Conflicts(shared,shared)=false so readers coexist, which means two subagents
// that stamped the SAME file (a write-set partition failure the LOTO_SUBAGENT_ID
// beacon exists to expose) never surface anywhere. The signal is in the data;
// this pushes it (loto-bo8c). Advisory: exits 0 even with collisions — it is a
// detection surface, and shared coexistence is legal; the ⚠ rows carry the heads-
// up. "Live" = not stale (PID probe for real pids, TTL for PID-0 beacons).
func statusCollisions(stdout, stderr io.Writer, rt *runtime) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe(), CaseFold: rt.CaseFold}
	ownersByTarget := map[string]map[string]bool{}
	for i := range all {
		l := &all[i]
		if ec.IsStale(*l) {
			continue
		}
		m := ownersByTarget[l.Target.Canonical]
		if m == nil {
			m = map[string]bool{}
			ownersByTarget[l.Target.Canonical] = m
		}
		m[string(l.OwnerUUID)] = true
	}

	type collision struct {
		target string
		owners []string
	}
	var cols []collision
	for tgt, owners := range ownersByTarget {
		if len(owners) < 2 {
			continue
		}
		os := make([]string, 0, len(owners))
		for o := range owners {
			os = append(os, o)
		}
		sort.Strings(os)
		cols = append(cols, collision{target: tgt, owners: os})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].target < cols[j].target })

	if len(cols) == 0 {
		fmt.Fprintln(stdout, "✓ no collisions")
		return 0
	}
	fmt.Fprintf(stdout, "⚠ collisions count=%d\n", len(cols))
	for _, c := range cols {
		fmt.Fprintf(stdout, "⚠ target=%s distinct_owners=%d owners=%s\n",
			relPath(c.target), len(c.owners), strings.Join(c.owners, ","))
	}
	return 0
}

func filterLocksByOwner(all []domain.LockRecord, ownerUUID string) []domain.LockRecord {
	filtered := all[:0]
	for i := range all {
		if string(all[i].OwnerUUID) == ownerUUID {
			filtered = append(filtered, all[i])
		}
	}
	return filtered
}

// printStatusLocks renders the locks section. mine gates the self marker
// (ferret-m3t): when the caller already asked for --mine, every remaining row
// is its own by construction and re-marking them would change output the AC
// pins as a golden diff, so the marker only prints in the unfiltered view.
// foreign reports whether any printed row belongs to someone else, so the
// caller can decide whether the actionable-findings fix block
// (emitForeignRowFixBlock) applies.
func printStatusLocks(stdout io.Writer, rt *runtime, all []domain.LockRecord, mine bool) (foreign bool) {
	if len(all) == 0 {
		fmt.Fprintln(stdout, "✓ no locks")
		return false
	}
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe(), CaseFold: rt.CaseFold}
	// Classify every row up front so the summary line and the per-row marks
	// agree on the same verdict (sd-kck) — a lock past its TTL backstop or
	// whose holder is provably gone is DEAD, and printing ✓ on it read as "53
	// healthy locks" when 49 of them were reclaimable rows nobody was
	// enforcing for. dead=%d makes the reclaim gap visible without opening a
	// single row; live/unknown fold together as "held" (both are non-stale —
	// Classify's own docstring).
	dead := 0
	for i := range all {
		if ec.Classify(all[i]) == domain.LivenessDead {
			dead++
		}
	}
	if dead == 0 {
		fmt.Fprintf(stdout, "✓ locks count=%d\n", len(all))
	} else {
		fmt.Fprintf(stdout, "⚠ locks count=%d held=%d expired=%d — reclaim: loto doctor --repair\n",
			len(all), len(all)-dead, dead)
	}
	canonicals := make([]domain.Canonical, len(all))
	for i := range all {
		canonicals[i] = domain.Canonical(all[i].Target.Canonical)
	}
	tagsByTarget, _ := rt.Store.ListAliveByTargets(rt.Ctx, canonicals)
	for i := range all {
		l := &all[i]
		// branch= names the tree the holder locked from (loto-16cf); omitted
		// when unrecorded (pre-16cf rows, non-git acquire paths).
		branch := ""
		if l.Branch != "" {
			branch = " branch=" + l.Branch
		}
		// mark distinguishes a DEAD row from a live/unknown one at a glance —
		// the same ✓ on every row (regardless of liveness=) is exactly the
		// defect: a reader scanning the left column saw 53 healthy locks when
		// 49 were expired, owner gone, file still 0444 (sd-kck).
		mark := "✓"
		verdict := ec.Classify(*l)
		if verdict == domain.LivenessDead {
			mark = "✗"
		}
		// self=true is the marker this bead adds (ferret-m3t): a row's owner_
		// uuid is opaque on its own, and an agent scanning the unfiltered dump
		// for its own reservation had no way to tell one uuid from another
		// without a second command. Derived from the exact identity `loto
		// whoami` reports — rt.Agent.UUID — so the two can never disagree, and
		// checked as a plain string equality regardless of which uuid space
		// this row's owner lives in: a claim/exclusive-lock row carries the
		// parent SESSION uuid, a `beacon:` row carries a PER-AGENT uuid, and
		// rt.Agent.UUID already resolves to whichever space the CALLING
		// process itself is in (identity.Ensure), so one comparison covers
		// both. Suppressed under --mine (see mine gate above the loop).
		self := ""
		if string(l.OwnerUUID) == rt.Agent.UUID {
			if !mine {
				self = " self=true"
			}
		} else {
			foreign = true
		}
		// epoch= is the generation half of this hold's identity: joined to
		// owner= as `owner@epoch` it is the token `unlock --force
		// --expect-holder` compares against (loto-tqcw). Printed as its own
		// field rather than a pre-joined `hold=` so no row repeats the owner.
		fmt.Fprintf(stdout, "%s target=%s owner=%s epoch=%d mode=%s intent=%q held_since=%s ttl_remaining=%s liveness=%s host=%s pid=%d%s%s\n",
			mark, relPath(l.Target.Canonical), l.OwnerUUID, l.Epoch, l.EffectiveMode(), l.Intent,
			l.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(ec.RemainingTTL(*l)), verdict,
			l.Host, l.PID, branch, self)
		render.EmitTagRows(stdout, tagsByTarget[l.Target.Canonical])
	}
	if dead > 0 {
		fmt.Fprintln(stdout, "‡ reclaim the expired locks above (restores file mode too):")
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintln(stdout, "loto doctor --repair")
		fmt.Fprintln(stdout, "```")
	}
	return foreign
}

// fmtTTL renders a remaining-TTL duration deterministically (whole seconds,
// "0s" when the backstop has fired). Avoids time.Duration's variable-precision
// String so status output is byte-stable for golden tests (design.md).
func fmtTTL(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d.Round(time.Second)/time.Second))
}

func statusSingleTarget(w io.Writer, rt *runtime, t domain.Target) int {
	// LocksAt scopes the query to this target (WHERE target_canonical = ?) in
	// deterministic order — same row set the old full-table ListLocks + Go-side
	// SameCanonical filter produced, without pulling every lock into memory.
	overlapping, err := rt.Store.LocksAt(rt.Ctx, t)
	if err != nil {
		fmt.Fprintf(w, "✗ %v\n", err)
		return 3
	}
	if len(overlapping) == 0 {
		fmt.Fprintf(w, "✓ free target=%s\n", relPath(t.Canonical))
		violationNoticeForPath(rt, w, t.Canonical)
		return 0
	}
	// ec is only consumed by the per-holder rows below; build it after the
	// no-overlap early return so the happy path skips the closure + time.Now.
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe(), CaseFold: rt.CaseFold}
	fmt.Fprintf(w, "✗ overlap count=%d target=%s\n", len(overlapping), relPath(t.Canonical))
	for i := range overlapping {
		l := &overlapping[i]
		// epoch= joins owner= into the `owner@epoch` --expect-holder token; this
		// is THE read a caller makes before deciding to break, so the token has
		// to be here (loto-tqcw).
		fmt.Fprintf(w, "✗ holder target=%s owner=%s epoch=%d mode=%s intent=%q ttl_remaining=%s liveness=%s\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.Epoch, l.EffectiveMode(), l.Intent,
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l))
	}
	if tags, err := rt.Store.ListAliveForTarget(rt.Ctx, domain.Canonical(t.Canonical)); err == nil {
		render.EmitTagRows(w, tags)
	}
	violationNoticeForPath(rt, w, t.Canonical)
	return 0
}
