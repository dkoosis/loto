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
	Path    string
	Blocker domain.LockRecord
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
	rows, invalid := computeCheckConflicts(paths, all, rt.Agent.UUID, repoTop, time.Now(), rt.Host, rt.liveProbe())
	if len(invalid) > 0 {
		printCheckInvalid(stdout, invalid)
		return 2
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	printCheckConflicts(stdout, rows)
	return 1
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

func computeCheckConflicts(paths []string, all []domain.LockRecord, myUUID, repoTop string, now time.Time, host string, live domain.PidLiveProbe) ([]checkConflict, []checkInvalid) {
	var rows []checkConflict
	var invalid []checkInvalid
	seen := map[string]bool{}
	for _, raw := range paths {
		t, err := resolveCLITarget(repoTop, raw)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		rows = appendCheckConflictsForTarget(rows, seen, t, all, myUUID, now, host, live)
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

func appendCheckConflictsForTarget(rows []checkConflict, seen map[string]bool, t domain.Target, all []domain.LockRecord, myUUID string, now time.Time, host string, live domain.PidLiveProbe) []checkConflict {
	for i := range all {
		l := &all[i]
		if l.OwnerUUID == myUUID || !domain.SameCanonical(l.Target, t) {
			continue
		}
		// A stale/dead-PID holder is reclaimable: AcquireLocks would silently
		// reclaim it (reclaimStaleAndCollectBlockers), so the proceed/block gate
		// must not report it as a hard conflict demanding `unlock --force`
		// (loto-9t0q).
		if domain.IsStale(*l, now, host, live) {
			continue
		}
		key := t.Canonical + "|" + l.Target.Canonical + "|" + l.OwnerUUID
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, checkConflict{Path: t.Canonical, Blocker: all[i]})
	}
	return rows
}

func printCheckConflicts(stdout io.Writer, rows []checkConflict) {
	fmt.Fprintf(stdout, "✗ conflicts count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(stdout, "✗ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s\n",
			relPath(r.Path), r.Blocker.OwnerUUID, relPath(r.Blocker.Target.Canonical), r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintf(stdout, "loto unlock --force -t \"unblock\" %s\n", relPath(r.Blocker.Target.Canonical))
		fmt.Fprintln(stdout, "```")
	}
}
