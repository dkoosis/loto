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

func init() { register("check-paths", cmdCheckPaths) } //nolint:gochecknoinits // command registry pattern

type checkPathsConflict struct {
	Path    string
	Blocker domain.LockRecord
}

func cmdCheckPaths(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-paths", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staged := fs.Bool("staged", false, "read paths from git diff --cached")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	paths, code := loadCheckPathsTargets(*staged, fs.Args(), stderr)
	if code != 0 {
		return code
	}
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "✓ no paths")
		return 0
	}

	rt, err := openRuntime()
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
	caseInsensitive := !runtimeCaseSensitive(rt)

	rows := computeCheckPathsConflicts(paths, all, rt.Agent.UUID, caseInsensitive)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	printCheckPathsConflicts(stdout, rows)
	return 1
}

func loadCheckPathsTargets(staged bool, posArgs []string, stderr io.Writer) ([]string, int) {
	if !staged {
		return posArgs, 0
	}
	out, err := exec.CommandContext(context.Background(), "git", "diff", "--cached", "--name-only", "-z").Output()
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

func runtimeCaseSensitive(rt *runtime) bool {
	repoTop, err := repoTopForCwd()
	if err != nil {
		return true
	}
	cs, err := rt.Store.FSCaseSensitive(repoTop)
	if err != nil {
		return true
	}
	return cs
}

func computeCheckPathsConflicts(paths []string, all []domain.LockRecord, myUUID string, caseInsensitive bool) []checkPathsConflict {
	var rows []checkPathsConflict
	seen := map[string]bool{}
	for _, p := range paths {
		rows = appendPathConflicts(rows, seen, p, all, myUUID, caseInsensitive)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Blocker.Target.Canonical < rows[j].Blocker.Target.Canonical
	})
	return rows
}

func appendPathConflicts(rows []checkPathsConflict, seen map[string]bool, p string, all []domain.LockRecord, myUUID string, caseInsensitive bool) []checkPathsConflict {
	t, err := domain.Canonicalize(p)
	if err != nil {
		return rows
	}
	for i := range all {
		l := &all[i]
		if l.OwnerUUID == myUUID || !domain.Overlap(l.Target, t, caseInsensitive) {
			continue
		}
		key := p + "|" + l.Target.Canonical + "|" + l.OwnerUUID
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, checkPathsConflict{Path: p, Blocker: all[i]})
	}
	return rows
}

func printCheckPathsConflicts(stdout io.Writer, rows []checkPathsConflict) {
	fmt.Fprintf(stdout, "✗ conflicts count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(stdout, "⚠ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s\n",
			r.Path, r.Blocker.OwnerUUID, r.Blocker.Target.Canonical, r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintf(stdout, "loto break --force --reason \"unblock\" %s\n", r.Blocker.Target.Canonical)
		fmt.Fprintln(stdout, "```")
	}
}
