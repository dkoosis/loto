package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
)

func init() { register("status", cmdStatus) } //nolint:gochecknoinits // command registry pattern

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mine := fs.Bool("mine", false, "show only locks owned by my uuid")
	session := fs.Bool("session", false, "alias for --mine in v2 (no per-session distinction)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	repoTop, _ := repoTopForCwd()
	fmt.Fprintf(stdout, "project: %s\n", ProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", rt.StateDir)

	// Single-target form: one positional arg.
	if fs.NArg() == 1 {
		t, err := domain.Canonicalize(fs.Arg(0))
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
	if *mine || *session {
		all = filterLocksByOwner(all, rt.Agent.UUID)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Target.Canonical != all[j].Target.Canonical {
			return all[i].Target.Canonical < all[j].Target.Canonical
		}
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})
	printStatusLocks(stdout, all)
	return 0
}

func filterLocksByOwner(all []domain.LockRecord, ownerUUID string) []domain.LockRecord {
	filtered := all[:0]
	for i := range all {
		if all[i].OwnerUUID == ownerUUID {
			filtered = append(filtered, all[i])
		}
	}
	return filtered
}

func printStatusLocks(stdout io.Writer, all []domain.LockRecord) {
	if len(all) == 0 {
		fmt.Fprintln(stdout, "✓ no locks")
		return
	}
	fmt.Fprintf(stdout, "ℹ locks count=%d\n", len(all))
	for i := range all {
		l := &all[i]
		fmt.Fprintf(stdout, "ℹ target=%s owner=%s intent=%q held_since=%s expires_at=%s host=%s pid=%d\n",
			l.Target.Canonical, l.OwnerUUID, l.Intent,
			l.CreatedAt.UTC().Format(time.RFC3339), l.ExpiresAt.UTC().Format(time.RFC3339),
			l.Host, l.PID)
	}
}

func statusSingleTarget(w io.Writer, rt *runtime, t domain.Target) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(w, "✗ %v\n", err)
		return 3
	}
	caseSensitive := true
	if repoTop, err := repoTopForCwd(); err == nil {
		if cs, err := rt.Store.FSCaseSensitive(repoTop); err == nil {
			caseSensitive = cs
		}
	}
	caseInsensitive := !caseSensitive

	var overlapping []domain.LockRecord
	for i := range all {
		if domain.Overlap(all[i].Target, t, caseInsensitive) {
			overlapping = append(overlapping, all[i])
		}
	}
	sort.Slice(overlapping, func(i, j int) bool {
		return overlapping[i].Target.Canonical < overlapping[j].Target.Canonical
	})
	if len(overlapping) == 0 {
		fmt.Fprintf(w, "✓ free target=%s\n", t.Canonical)
		return 0
	}
	fmt.Fprintf(w, "ℹ overlap count=%d target=%s\n", len(overlapping), t.Canonical)
	for i := range overlapping {
		l := &overlapping[i]
		fmt.Fprintf(w, "ℹ holder target=%s owner=%s intent=%q expires_at=%s\n",
			l.Target.Canonical, l.OwnerUUID, l.Intent, l.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return 0
}
