package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/store"
)

func init() { register("doctor", cmdDoctor) } //nolint:gochecknoinits // command registry pattern

func renderDoctorReport(stdout io.Writer, report *store.DoctorReport) {
	sort.Slice(report.StaleLocks, func(i, j int) bool {
		return report.StaleLocks[i].Target.Canonical < report.StaleLocks[j].Target.Canonical
	})
	sort.Slice(report.SidecarFindings, func(i, j int) bool {
		if report.SidecarFindings[i].Target != report.SidecarFindings[j].Target {
			return report.SidecarFindings[i].Target < report.SidecarFindings[j].Target
		}
		return report.SidecarFindings[i].Reason < report.SidecarFindings[j].Reason
	})
	if len(report.StaleLocks) == 0 && len(report.SidecarFindings) == 0 && report.IntegrityOK {
		fmt.Fprintln(stdout, "✓ healthy")
		return
	}
	fmt.Fprintf(stdout, "ℹ stale_locks=%d sidecar_findings=%d integrity=%s\n",
		len(report.StaleLocks), len(report.SidecarFindings), report.IntegrityDetail)
	for i := range report.StaleLocks {
		l := &report.StaleLocks[i]
		fmt.Fprintf(stdout, "⚠ stale target=%s owner=%s expires_at=%s host=%s pid=%d\n",
			l.Target.Canonical, l.OwnerUUID, l.ExpiresAt.UTC().Format(time.RFC3339), l.Host, l.PID)
	}
	for i := range report.SidecarFindings {
		f := &report.SidecarFindings[i]
		if f.Detail != "" {
			fmt.Fprintf(stdout, "⚠ zombie_held target=%s pid=%d reason=%s cwd=%s\n",
				f.Target, f.PID, f.Reason, f.Detail)
		} else {
			fmt.Fprintf(stdout, "⚠ zombie_held target=%s pid=%d reason=%s\n",
				f.Target, f.PID, f.Reason)
		}
	}
}

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

	repoTop, _ := repoTopForCwd()
	report, err := rt.Store.DoctorAuditWith(rt.Ctx, rt.Host, live, store.SidecarCheck{
		SidecarDir: store.DefaultSidecarDir(),
		RepoTop:    repoTop,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	fmt.Fprintf(stdout, "project: %s\n", ProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", rt.StateDir)

	renderDoctorReport(stdout, report)

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
