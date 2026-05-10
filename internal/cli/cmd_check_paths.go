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

func init() { register("check-paths", cmdCheckPaths) }

func cmdCheckPaths(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-paths", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staged := fs.Bool("staged", false, "read paths from git diff --cached")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	var paths []string
	if *staged {
		out, err := exec.CommandContext(context.Background(), "git", "diff", "--cached", "--name-only", "-z").Output()
		if err != nil {
			fmt.Fprintf(stderr, "✗ git diff: %v\n", err)
			return 3
		}
		for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
	} else {
		paths = fs.Args()
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
	caseSensitive := true
	if repoTop, err := repoTopForCwd(); err == nil {
		if cs, err := rt.Store.FSCaseSensitive(repoTop); err == nil {
			caseSensitive = cs
		}
	}
	caseInsensitive := !caseSensitive

	type conflictRow struct {
		Path    string
		Blocker domain.LockRecord
	}
	var rows []conflictRow
	seen := map[string]bool{}
	for _, p := range paths {
		t, err := domain.Canonicalize(p)
		if err != nil {
			continue
		}
		for _, l := range all {
			if l.OwnerUUID == rt.Agent.UUID {
				continue
			}
			if !domain.Overlap(l.Target, t, caseInsensitive) {
				continue
			}
			key := p + "|" + l.Target.Canonical + "|" + l.OwnerUUID
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, conflictRow{Path: p, Blocker: l})
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ no conflicts")
		return 0
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].Blocker.Target.Canonical < rows[j].Blocker.Target.Canonical
	})
	fmt.Fprintf(stdout, "✗ conflicts count=%d\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(stdout, "⚠ path=%s blocker=%s holder_target=%s intent=%q expires_at=%s\n",
			r.Path, r.Blocker.OwnerUUID, r.Blocker.Target.Canonical, r.Blocker.Intent,
			r.Blocker.ExpiresAt.UTC().Format(time.RFC3339))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintf(stdout, "loto break --force --reason \"unblock\" %s\n", r.Blocker.Target.Canonical)
		fmt.Fprintln(stdout, "```")
	}
	return 1
}
