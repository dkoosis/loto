package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"
)

func init() { register("doctor", cmdDoctor) } //nolint:gochecknoinits // command registry pattern

func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repair := fs.Bool("repair", false, "reclaim stale locks")
	dryRun := fs.Bool("dry-run", false, "report what --repair would do, without writing")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	live := func(host string, pid int) bool {
		if host != rt.Host {
			return true
		}
		return pidLive(pid)
	}

	report, err := rt.Store.DoctorAudit(rt.Ctx, rt.Host, live)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	repoTop, _ := repoTopForCwd()
	fmt.Fprintf(stdout, "project: %s\n", ProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", rt.StateDir)

	sort.Slice(report.StaleLocks, func(i, j int) bool {
		return report.StaleLocks[i].Target.Canonical < report.StaleLocks[j].Target.Canonical
	})
	if len(report.StaleLocks) == 0 && report.IntegrityOK {
		fmt.Fprintln(stdout, "✓ healthy")
	} else {
		fmt.Fprintf(stdout, "ℹ stale_locks=%d integrity=%s\n",
			len(report.StaleLocks), report.IntegrityDetail)
		for i := range report.StaleLocks {
			l := &report.StaleLocks[i]
			fmt.Fprintf(stdout, "⚠ stale target=%s owner=%s expires_at=%s host=%s pid=%d\n",
				l.Target.Canonical, l.OwnerUUID, l.ExpiresAt.UTC().Format(time.RFC3339), l.Host, l.PID)
		}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "ℹ dry-run would_reclaim=%d\n", len(report.StaleLocks))
		return 0
	}
	if *repair {
		if err := rt.Store.DoctorRepair(rt.Ctx, rt.Host, rt.Agent.UUID, live); err != nil {
			fmt.Fprintf(stderr, "✗ repair: %v\n", err)
			return 3
		}
		fmt.Fprintln(stdout, "✓ repaired")
	}
	return 0
}
