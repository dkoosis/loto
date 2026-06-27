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
	staleLocks := append([]domain.LockRecord(nil), report.StaleLocks...)
	sort.Slice(staleLocks, func(i, j int) bool {
		return staleLocks[i].Target.Canonical < staleLocks[j].Target.Canonical
	})
	sidecarFindings := append([]store.SidecarFinding(nil), report.SidecarFindings...)
	sort.Slice(sidecarFindings, func(i, j int) bool {
		if sidecarFindings[i].Target != sidecarFindings[j].Target {
			return sidecarFindings[i].Target < sidecarFindings[j].Target
		}
		return sidecarFindings[i].Reason < sidecarFindings[j].Reason
	})
	if len(staleLocks) == 0 && len(sidecarFindings) == 0 && report.IntegrityOK {
		fmt.Fprintln(stdout, "✓ healthy")
		return
	}
	fmt.Fprintf(stdout, "✗ stale_locks=%d sidecar_findings=%d integrity=%s\n",
		len(staleLocks), len(sidecarFindings), report.IntegrityDetail)
	for i := range staleLocks {
		l := &staleLocks[i]
		fmt.Fprintf(stdout, "✗ stale target=%s owner=%s expires_at=%s host=%s pid=%d\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.ExpiresAt.UTC().Format(time.RFC3339), l.Host, l.PID)
	}
	for i := range sidecarFindings {
		f := &sidecarFindings[i]
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
	defer rt.DeferredTagFooter(stdout)

	live := rt.liveProbe()

	repoTop, _ := repoTopForCwd(ctx)
	report, err := rt.Store.DoctorAudit(rt.Ctx, rt.Host, live, store.SidecarCheck{
		SidecarDir: store.DefaultSidecarDir(),
		RepoTop:    repoTop,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	fmt.Fprintf(stdout, "project: %s\n", ResolveAndPinProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", rt.StateDir)

	renderDoctorReport(stdout, report)

	orphans, scanIncomplete := scanOrphansAndHint(rt, repoTop, orphanFlags{
		orphanMode:    *orphanMode,
		restoreOrphan: *restoreOrphan,
		repair:        *repair,
	}, stdout)

	if *dryRun {
		fmt.Fprintf(stdout, "✓ dry-run would_reclaim=%d\n", len(report.StaleLocks))
		if scanIncomplete {
			return 3
		}
		return 0
	}
	if *repair {
		if code := doRepair(rt, live, *restoreOrphan, orphans, stdout, stderr); code != 0 {
			return code
		}
	}
	if scanIncomplete {
		return 3
	}
	return 0
}

// orphanFlags bundles the doctor flags that gate orphan-mode scanning.
type orphanFlags struct {
	orphanMode    bool
	restoreOrphan bool
	repair        bool
}

// scanOrphansAndHint runs the orphan-mode scan when requested and prints the
// restore-recovery hint. It returns the orphan list and whether the scan was
// incomplete (gh#130). Factored out of cmdDoctor to keep its complexity in check.
func scanOrphansAndHint(rt *runtime, repoTop string, f orphanFlags, stdout io.Writer) (orphans []string, scanIncomplete bool) {
	if !f.orphanMode && !f.restoreOrphan {
		return nil, false
	}
	orphans, scanIncomplete = runOrphanScan(rt, repoTop, stdout)
	// Surface the recovery path. An orphan-mode file is read-only with no lock
	// row (e.g. a SIGKILL between strip and commit in lock acquire, loto-j863):
	// a dead-end unless the user knows the restore flag. Suppress when this run
	// is already restoring — the repair line below says it all.
	if len(orphans) > 0 && (!f.repair || !f.restoreOrphan) {
		fmt.Fprintf(stdout, "‡ %d orphan-mode file(s) read-only with no lock row — restore writable:\n", len(orphans))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintln(stdout, "loto doctor --repair --restore-orphan-mode")
		fmt.Fprintln(stdout, "```")
	}
	return orphans, scanIncomplete
}

func doRepair(rt *runtime, live domain.PidLiveProbe, restoreOrphan bool, orphans []string, stdout, stderr io.Writer) int {
	if err := rt.Store.DoctorRepair(rt.Ctx, rt.Host, rt.Agent.UUID, live); err != nil {
		fmt.Fprintf(stderr, "✗ repair: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "✓ repaired")
	if restoreOrphan && len(orphans) > 0 {
		restored, failures, err := rt.Store.RestoreOrphanMode(rt.Ctx, orphans)
		if err != nil {
			fmt.Fprintf(stderr, "✗ restore-orphan-mode: %v\n", err)
			return 3
		}
		fmt.Fprintf(stdout, "✓ restored-orphan-mode count=%d failed=%d\n", len(restored), len(failures))
		for _, f := range failures {
			fmt.Fprintf(stdout, "✗ restore-orphan-mode target=%s err=%v\n", f.Path, f.Err)
		}
	}
	return 0
}

// runOrphanScan performs the orphan-mode scan and reports any incomplete-scan
// signal (gh#130). Returns the orphan list and a flag set when the underlying
// walk skipped entries (e.g. permission-denied subtrees) so the caller can
// surface a non-zero exit instead of a false-clean report.
func runOrphanScan(rt *runtime, repoTop string, stdout io.Writer) ([]string, bool) {
	orphans, skipped, firstErr := scanAndReportOrphans(rt, repoTop, stdout)
	if skipped > 0 {
		fmt.Fprintf(stdout, "✗ scan-skipped count=%d first=%v\n", skipped, firstErr)
		return orphans, true
	}
	return orphans, false
}

// scanAndReportOrphans walks the repo for orphan-mode candidates. It returns
// the orphan list, a count of walk entries skipped due to errors (e.g.
// permission-denied subtrees), and the first walk error encountered. Callers
// must surface a non-zero skipped count as an incomplete-scan signal; silently
// dropping these would produce a false-clean report (gh#130).
func scanAndReportOrphans(rt *runtime, repoTop string, stdout io.Writer) ([]string, int, error) {
	candidates, skipped, firstWalkErr := walkRepoCandidates(repoTop)
	orphans, err := rt.Store.ScanOrphanModes(rt.Ctx, candidates)
	if err != nil {
		fmt.Fprintf(stdout, "✗ scan-orphans: %v\n", err)
		return nil, skipped, firstWalkErr
	}
	for _, p := range orphans {
		rel, err := filepath.Rel(repoTop, p)
		if err != nil {
			rel = p
		}
		fmt.Fprintf(stdout, "✗ orphan-mode target=%s\n", rel)
	}
	return orphans, skipped, firstWalkErr
}

var walkSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".cache": true,
}

// walkRepoCandidates enumerates regular files under root, skipping known
// vendored/build dirs. Walk errors (typically permission-denied subtrees) are
// counted via skipped and the first such error is returned via firstErr — the
// caller is responsible for surfacing an incomplete-scan signal so a partial
// walk does not masquerade as a clean result (gh#130).
func walkRepoCandidates(root string) (out []string, skipped int, firstErr error) {
	if root == "" {
		return nil, 0, nil
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped++
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p, err)
			}
			// Continue the walk so a single unreadable subtree does not abort
			// the scan; the caller reports the partial result via skipped/firstErr.
			return nil
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
	return out, skipped, firstErr
}
