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

	paths, code := loadCheckTargets(ctx, *staged, fs.Args(), stderr)
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

	all, err := rt.Locks().ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	repoTop, _ := repoTopForCwd(ctx)
	rows, invalid := computeCheckConflicts(paths, all, rt.Agent.UUID, repoTop)
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

func loadCheckTargets(ctx context.Context, staged bool, posArgs []string, stderr io.Writer) ([]string, int) {
	if !staged {
		return posArgs, 0
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z").Output()
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

func computeCheckConflicts(paths []string, all []domain.LockRecord, myUUID, repoTop string) ([]checkConflict, []checkInvalid) {
	var rows []checkConflict
	var invalid []checkInvalid
	seen := map[string]bool{}
	for _, raw := range paths {
		p := normalizeRepoPath(raw, repoTop)
		t, err := domain.Canonicalize(p)
		if err != nil {
			invalid = append(invalid, checkInvalid{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		rows = appendCheckConflictsForTarget(rows, seen, p, t, all, myUUID)
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

func appendCheckConflictsForTarget(rows []checkConflict, seen map[string]bool, p string, t domain.Target, all []domain.LockRecord, myUUID string) []checkConflict {
	for i := range all {
		l := &all[i]
		if l.OwnerUUID == myUUID || !domain.Overlap(l.Target, t) {
			continue
		}
		key := p + "|" + l.Target.Canonical + "|" + l.OwnerUUID
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, checkConflict{Path: p, Blocker: all[i]})
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
