package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
)

func init() { register("status", cmdStatus) } //nolint:gochecknoinits // command registry pattern

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mine := fs.Bool("mine", false, "show only locks owned by my uuid")
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
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	canonicals := make([]domain.Canonical, len(all))
	for i := range all {
		canonicals[i] = domain.Canonical(all[i].Target.Canonical)
	}
	tagsByTarget, _ := rt.Store.ListAliveByTargets(rt.Ctx, canonicals)
	for i := range all {
		l := &all[i]
		fmt.Fprintf(stdout, "✓ target=%s owner=%s mode=%s intent=%q held_since=%s ttl_remaining=%s liveness=%s host=%s pid=%d\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.EffectiveMode(), l.Intent,
			l.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l),
			l.Host, l.PID)
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
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(w, "✗ %v\n", err)
		return 3
	}
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	var overlapping []domain.LockRecord
	for i := range all {
		if domain.SameCanonical(all[i].Target, t) {
			overlapping = append(overlapping, all[i])
		}
	}
	sort.Slice(overlapping, func(i, j int) bool {
		return overlapping[i].Target.Canonical < overlapping[j].Target.Canonical
	})
	if len(overlapping) == 0 {
		fmt.Fprintf(w, "✓ free target=%s\n", relPath(t.Canonical))
		return 0
	}
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
