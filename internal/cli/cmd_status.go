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

	if *collisions {
		return statusCollisions(stdout, stderr, rt)
	}

	if fs.NArg() == 1 {
		t, err := resolveCLITarget(repoTop, fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 2
		}
		return statusSingleTarget(stdout, rt, t)
	}

	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if *mine {
		all = filterLocksByOwner(all, rt.Agent.UUID)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Target.Canonical != all[j].Target.Canonical {
			return all[i].Target.Canonical < all[j].Target.Canonical
		}
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})
	printStatusLocks(stdout, rt, all)

	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	printStatusClaims(stdout, rt, claims, *mine, time.Now())
	return 0
}

// printStatusClaims renders the claims section after locks (loto-7af9): live
// rows only — Expired is display-time authority; the row itself dies lazily in
// a later overlapping acquire — sorted prefix then created_at, --mine honored.
// Explicit empty header per design.md: silence looks like a crash.
func printStatusClaims(stdout io.Writer, rt *runtime, all []domain.ClaimRecord, mine bool, now time.Time) {
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
		return
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
		fmt.Fprintf(stdout, "✓ prefix=%s owner=%s intent=%q held_since=%s ttl_remaining=%s host=%s\n",
			relPath(c.PathPrefix), c.OwnerUUID, c.Intent,
			c.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(c.ExpiresAt.Sub(now)), c.Host)
	}
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
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}
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

func printStatusLocks(stdout io.Writer, rt *runtime, all []domain.LockRecord) {
	if len(all) == 0 {
		fmt.Fprintln(stdout, "✓ no locks")
		return
	}
	fmt.Fprintf(stdout, "✓ locks count=%d\n", len(all))
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}
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
		fmt.Fprintf(stdout, "✓ target=%s owner=%s mode=%s intent=%q held_since=%s ttl_remaining=%s liveness=%s host=%s pid=%d%s\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.EffectiveMode(), l.Intent,
			l.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l),
			l.Host, l.PID, branch)
		render.EmitTagRows(stdout, tagsByTarget[l.Target.Canonical])
	}
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
		return 0
	}
	// ec is only consumed by the per-holder rows below; build it after the
	// no-overlap early return so the happy path skips the closure + time.Now.
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}
	fmt.Fprintf(w, "✗ overlap count=%d target=%s\n", len(overlapping), relPath(t.Canonical))
	for i := range overlapping {
		l := &overlapping[i]
		fmt.Fprintf(w, "✗ holder target=%s owner=%s mode=%s intent=%q ttl_remaining=%s liveness=%s\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.EffectiveMode(), l.Intent,
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l))
	}
	if tags, err := rt.Store.ListAliveForTarget(rt.Ctx, domain.Canonical(t.Canonical)); err == nil {
		render.EmitTagRows(w, tags)
	}
	return 0
}
