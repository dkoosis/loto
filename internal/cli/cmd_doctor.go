package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"loto/internal/domain"
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
	fmt.Fprintf(stdout, "✗ stale_locks=%d sidecar_findings=%d integrity=%s\n",
		len(report.StaleLocks), len(report.SidecarFindings), report.IntegrityDetail)
	for i := range report.StaleLocks {
		l := &report.StaleLocks[i]
		fmt.Fprintf(stdout, "✗ stale target=%s owner=%s expires_at=%s host=%s pid=%d\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.ExpiresAt.UTC().Format(time.RFC3339), l.Host, l.PID)
	}
	for i := range report.SidecarFindings {
		f := &report.SidecarFindings[i]
		if f.Detail != "" {
			fmt.Fprintf(stdout, "✗ zombie_held target=%s pid=%d reason=%s cwd=%s\n",
				relPath(f.Target), f.PID, f.Reason, f.Detail)
		} else {
			fmt.Fprintf(stdout, "✗ zombie_held target=%s pid=%d reason=%s\n",
				relPath(f.Target), f.PID, f.Reason)
		}
	}
}

func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repair := fs.Bool("repair", false, "reclaim stale locks")
	dryRun := fs.Bool("dry-run", false, "report what --repair would do, without writing")
	orphanMode := fs.Bool("orphan-mode", false, "scan for orphan-mode files and report them")
	restoreOrphan := fs.Bool("restore-orphan-mode", false, "with --repair, also restore writable mode on orphan-mode files (implies --orphan-mode)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime(ctx)
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

	repoTop, _ := repoTopForCwd(ctx)
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

	var orphans []string
	if *orphanMode || *restoreOrphan {
		orphans = scanAndReportOrphans(rt, repoTop, stdout)
	}

	if *dryRun {
		fmt.Fprintf(stdout, "✓ dry-run would_reclaim=%d\n", len(report.StaleLocks))
		return 0
	}
	if *repair {
		if code := doRepair(rt, live, *restoreOrphan, orphans, stdout, stderr); code != 0 {
			return code
		}
	}
	return 0
}

func doRepair(rt *runtime, live domain.PidLiveProbe, restoreOrphan bool, orphans []string, stdout, stderr io.Writer) int {
	if err := rt.Store.DoctorRepair(rt.Ctx, rt.Host, rt.Agent.UUID, live); err != nil {
		fmt.Fprintf(stderr, "✗ repair: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "✓ repaired")
	if restoreOrphan && len(orphans) > 0 {
		restored, failures := rt.Store.RestoreOrphanMode(orphans)
		fmt.Fprintf(stdout, "✓ restored-orphan-mode count=%d failed=%d\n", len(restored), len(failures))
		for _, f := range failures {
			fmt.Fprintf(stdout, "✗ restore-orphan-mode target=%s err=%v\n", f.Path, f.Err)
		}
	}
	return 0
}

func scanAndReportOrphans(rt *runtime, repoTop string, stdout io.Writer) []string {
	candidates := walkRepoCandidates(repoTop)
	orphans, err := rt.Store.ScanOrphanModes(rt.Ctx, candidates)
	if err != nil {
		fmt.Fprintf(stdout, "✗ scan-orphans: %v\n", err)
		return nil
	}
	for _, p := range orphans {
		rel, err := filepath.Rel(repoTop, p)
		if err != nil {
			rel = p
		}
		fmt.Fprintf(stdout, "✗ orphan-mode target=%s\n", rel)
	}
	return orphans
}

var walkSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".cache": true,
}

func walkRepoCandidates(root string) []string {
	if root == "" {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, continue walk
		}
		if d.IsDir() {
			if walkSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}
