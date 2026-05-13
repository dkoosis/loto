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

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staged := fs.Bool("staged", false, "read paths from git diff --cached")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	paths, code := loadCheckTargets(*staged, fs.Args(), stderr)
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

	rows := computeCheckConflicts(paths, all, rt.Agent.UUID, caseInsensitive)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	printCheckConflicts(stdout, rows)
	return 1
}

func loadCheckTargets(staged bool, posArgs []string, stderr io.Writer) ([]string, int) {
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

func computeCheckConflicts(paths []string, all []domain.LockRecord, myUUID string, caseInsensitive bool) []checkConflict {
	var rows []checkConflict
	seen := map[string]bool{}
	for _, p := range paths {
		rows = appendCheckConflicts(rows, seen, p, all, myUUID, caseInsensitive)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Blocker.Target.Canonical < rows[j].Blocker.Target.Canonical
	})
	return rows
}

func appendCheckConflicts(rows []checkConflict, seen map[string]bool, p string, all []domain.LockRecord, myUUID string, caseInsensitive bool) []checkConflict {
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
		rows = append(rows, checkConflict{Path: p, Blocker: all[i]})
	}
	return rows
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

func printCheckConflicts(stdout io.Writer, rows []checkConflict) {
	fmt.Fprintf(stdout, "✗ conflicts count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(stdout, "⚠ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s\n",
			r.Path, r.Blocker.OwnerUUID, r.Blocker.Target.Canonical, r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintf(stdout, "loto unlock --force -t \"unblock\" %s\n", r.Blocker.Target.Canonical)
		fmt.Fprintln(stdout, "```")
	}
}
