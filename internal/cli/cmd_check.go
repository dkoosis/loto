package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
)

func init() { register("check", cmdCheck) } //nolint:gochecknoinits // command registry pattern

type checkConflict struct {
	Path     string
	Blocker  domain.LockRecord
	Blocking bool // true: provably-live exclusive holder → hard block (exit 1).
	// false: indeterminate/expiring liveness → advisory warn (does not set exit 1).
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staged := fs.Bool("staged", false, "read paths from git diff --cached")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	// Resolve repoTop before shelling out to git so `git diff --cached` runs
	// with cmd.Dir = repoTop (loto-jff, gh#128). Without that pin, the staged
	// diff inherits process cwd and reads the wrong repo's index in worktree /
	// nested-launch / scripted-invocation scenarios.
	repoTop, _ := repoTopForCwd(ctx)

	paths, code := loadCheckTargets(ctx, repoTop, *staged, fs.Args(), stderr)
	if code != 0 {
		return code
	}
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "✓ no paths")
		return 0
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	// Filter stale/dead-PID holders the same way AcquireLocks does
	// (reclaimStaleAndCollectBlockers → domain.IsStale): a lock that `loto lock`
	// would silently reclaim must not read as a hard conflict here (loto-9t0q).
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	rows, invalid := computeCheckConflicts(paths, all, rt.Agent.UUID, repoTop, ec)
	if len(invalid) > 0 {
		printCheckInvalid(stdout, invalid)
		return 2
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	if printCheckConflicts(stdout, rows) {
		return 1
	}
	return 0
}

type checkInvalid struct {
	Path   string
	Reason string
}

func printCheckInvalid(stdout io.Writer, rows []checkInvalid) {
	fmt.Fprintf(stdout, "✗ invalid count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(stdout, "✗ path=%s reason=%s\n", r.Path, r.Reason)
	}
}

func loadCheckTargets(ctx context.Context, repoTop string, staged bool, posArgs []string, stderr io.Writer) ([]string, int) {
	if !staged {
		return posArgs, 0
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	// Pin cmd.Dir = repoTop so the staged diff comes from the loto-resolved
	// repo, not from process cwd (loto-jff, gh#128).
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z")
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(stderr, "✗ git diff: %v\n", err)
		return nil, 3
	}
	var paths []string
	for p := range strings.SplitSeq(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, 0
}

func computeCheckConflicts(paths []string, all []domain.LockRecord, myUUID, repoTop string, ec domain.EvalContext) ([]checkConflict, []checkInvalid) {
	var rows []checkConflict
	var invalid []checkInvalid
	seen := map[string]bool{}
	for _, raw := range paths {
		t, err := resolveCLITarget(repoTop, raw)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		rows = appendCheckConflictsForTarget(rows, seen, t, all, myUUID, ec)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Blocker.Target.Canonical < rows[j].Blocker.Target.Canonical
	})
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Path < invalid[j].Path })
	return rows, invalid
}

func appendCheckConflictsForTarget(rows []checkConflict, seen map[string]bool, t domain.Target, all []domain.LockRecord, myUUID string, ec domain.EvalContext) []checkConflict {
	// Probe as SHARED, not exclusive: the committer's question is "does any peer
	// hold a lease that excludes me?" Conflicts(shared, shared)=false (readers
	// coexist) and Conflicts(shared, exclusive)=true — so only an exclusive peer
	// conflicts, which is the intended check semantics (loto-k5el.2 T8). A stale
	// holder is filtered inside Conflicts (AcquireLocks would reclaim it; the
	// gate must not demand `unlock --force` for a reclaimable lock, loto-9t0q).
	probe := domain.LockRecord{Target: t, OwnerUUID: domain.AgentUUID(myUUID), Mode: domain.ModeShared}
	for i := range all {
		l := &all[i]
		if !ec.Conflicts(probe, *l) {
			continue
		}
		key := t.Canonical + "|" + l.Target.Canonical + "|" + string(l.OwnerUUID)
		if seen[key] {
			continue
		}
		seen[key] = true
		// Liveness gate (binding correction 4 / §check --staged): a provably-live
		// exclusive holder hard-blocks (exit 1); an indeterminate/expiring one
		// (PID-0 sentinel, cross-host) is an advisory warn that does not block.
		// Classify is .1's display-tier verdict (loto-k5el.1).
		blocking := ec.Classify(*l) == domain.LivenessAlive
		rows = append(rows, checkConflict{Path: t.Canonical, Blocker: all[i], Blocking: blocking})
	}
	return rows
}

// printCheckConflicts renders one row per conflict and returns whether any row
// is a hard blocker (provably-live exclusive holder). Blocking rows lead with ✗
// and carry an actionable force-unlock fix block; advisory rows lead with ✓
// (the committer may proceed) and a liveness=unknown field — they do not set
// exit 1 (loto-k5el.2 T8). The closed glyph vocabulary (design.md) is ✓/✗ only,
// so "warn" is signalled by ✓ + liveness=unknown rather than a third glyph.
func printCheckConflicts(stdout io.Writer, rows []checkConflict) bool {
	blocking := 0
	for i := range rows {
		if rows[i].Blocking {
			blocking++
		}
	}
	head := "✓"
	if blocking > 0 {
		head = "✗"
	}
	fmt.Fprintf(stdout, "%s conflicts count=%d blocking=%d\n", head, len(rows), blocking)
	for i := range rows {
		r := &rows[i]
		if r.Blocking {
			fmt.Fprintf(stdout, "✗ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s liveness=alive\n",
				relPath(r.Path), r.Blocker.OwnerUUID, relPath(r.Blocker.Target.Canonical), r.Blocker.Intent,
				r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
			fmt.Fprintln(stdout, "```bash")
			fmt.Fprintf(stdout, "loto unlock --force -t \"unblock\" %s\n", relPath(r.Blocker.Target.Canonical))
			fmt.Fprintln(stdout, "```")
			continue
		}
		fmt.Fprintf(stdout, "✓ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s liveness=unknown\n",
			relPath(r.Path), r.Blocker.OwnerUUID, relPath(r.Blocker.Target.Canonical), r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return blocking > 0
}
