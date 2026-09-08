package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
	"loto/internal/identity"
	"loto/internal/render"
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
	// A note that lapsed with nobody having read it is a finding, so a box whose
	// only trouble is one of those must not print ✓ healthy (loto-z3y1 D2).
	if len(staleLocks) == 0 && len(report.ExpiredClaims) == 0 && len(report.StaleCandidateClaims) == 0 &&
		len(sidecarFindings) == 0 && len(report.ExpiredTerritoryTags) == 0 && report.IntegrityOK {
		fmt.Fprintln(stdout, "✓ healthy")
		return
	}
	fmt.Fprintf(stdout, "✗ stale_locks=%d expired_claims=%d stale_candidate_claims=%d expired_territory_tags=%d sidecar_findings=%d integrity=%s candidate_claim_grace=%s\n",
		len(staleLocks), len(report.ExpiredClaims), len(report.StaleCandidateClaims), len(report.ExpiredTerritoryTags),
		len(sidecarFindings), report.IntegrityDetail, domain.CandidateClaimReclaimGrace)
	for i := range staleLocks {
		l := &staleLocks[i]
		fmt.Fprintf(stdout, "✗ stale target=%s owner=%s expires_at=%s host=%s pid=%d\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.ExpiresAt.UTC().Format(time.RFC3339), l.Host, l.PID)
	}
	// ExpiredClaims arrive (prefix, owner)-sorted from DoctorAudit; prefixes
	// are stored repo-relative, so no relPath translation.
	for i := range report.ExpiredClaims {
		c := &report.ExpiredClaims[i]
		fmt.Fprintf(stdout, "✗ expired_claim prefix=%s owner=%s expires_at=%s\n",
			c.PathPrefix, c.OwnerUUID, c.ExpiresAt.UTC().Format(time.RFC3339))
	}
	// StaleCandidateClaims arrive in ListCandidateClaims' (path, candidate_id)
	// order — canonical paths are repo-relative, so relPath applies.
	for i := range report.StaleCandidateClaims {
		c := &report.StaleCandidateClaims[i]
		fmt.Fprintf(stdout, "✗ stale_candidate_claim candidate=%s target=%s session=%s created_at=%s pid=%d\n",
			c.CandidateID, relPath(c.PathCanonical), c.SessionUUID, c.CreatedAt.UTC().Format(time.RFC3339), c.PID)
	}
	// ⚠ rather than ✗: the note is not broken state, it is undelivered word,
	// and --repair will sweep it. The text rides along so the row is actionable
	// — a count alone tells the reader something was lost without telling them
	// what, which is the worst of both.
	render.EmitExpiredTerritoryTags(stdout, report.ExpiredTerritoryTags, time.Now())
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

// renderIdentityGC reports the session-directory reap doctor just ran
// (loto-6pn6). Suppressed entirely at zero/zero so a healthy box keeps
// today's byte-identical output — no new noise on the common path
// (help_golden_test / output_glyphs_test). `ℹ`: a data row, neither pass nor
// fail. `⚠` + fix block only fires when the bound (sessionGCMaxUnlink) left
// unreaped candidates for a later run.
func renderIdentityGC(stdout io.Writer, reaped, residual int) {
	if reaped == 0 && residual == 0 {
		return
	}
	fmt.Fprintf(stdout, "ℹ identity_gc sessions_reaped=%d sessions_residual=%d\n", reaped, residual)
	if residual > 0 {
		fmt.Fprintf(stdout, "⚠ identity_gc residual=%d — reap bounded at 5000/pass; re-run:\n", residual)
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintln(stdout, "loto doctor")
		fmt.Fprintln(stdout, "```")
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

	// Session-record hygiene. doctor forces an unconditional sweep, unlike
	// write verbs' GCSessionsIfDue (openRuntimeGC, sd-kx5) which skips most
	// calls via a marker file — this is the "fix it now" tool, so it must not
	// defer to that marker. The read path runs neither.
	sessionsReaped, sessionsResidual, _ := identity.GCSessions(time.Now(), string(rt.SessionUUID), lockOwnerUUIDs(ctx, rt.Store))

	live := rt.liveProbe()

	repoTop, _ := repoTopForCwd(ctx)
	report, err := rt.Store.DoctorAudit(rt.Ctx, rt.Host, rt.HostKnown, live, store.SidecarCheck{
		SidecarDir: store.DefaultSidecarDir(),
		RepoTop:    repoTop,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	printDoctorHeader(ctx, stdout, repoTop, rt.StateDir)
	renderIdentityGC(stdout, sessionsReaped, sessionsResidual)

	renderDoctorReport(stdout, report)

	// Dangling-stash report (loto-kotb): advisory only, never touched by
	// --repair below — D8 forbids doctor from popping/applying/dropping a
	// stash it did not create.
	renderDanglingStashes(stdout, time.Now(), scanDanglingStashes(rt.Ctx, repoTop, lockOwnerUUIDs(ctx, rt.Store)))

	residue := reportClaimResidue(rt, repoTop, stdout)

	orphans, scanIncomplete := scanOrphansAndHint(rt, repoTop, live, orphanFlags{
		orphanMode:    *orphanMode,
		restoreOrphan: *restoreOrphan,
		repair:        *repair,
	}, stdout)

	if *dryRun {
		// would_gc_claims mirrors the repair tx's gcClaimsTx sweep (D3);
		// would_gc_candidate_claims mirrors gcCandidateClaimsTx (loto-u2p7) — the
		// dry-run names everything --repair would delete, not just lock rows.
		// candidate_claim_grace names the age floor a zero-evidence claim must
		// clear before it counts (domain.CandidateClaimReclaimGrace, PR #298
		// review) — so an operator sees why a fresh one didn't.
		fmt.Fprintf(stdout, "✓ dry-run would_reclaim=%d would_gc_claims=%d would_gc_candidate_claims=%d would_release_residue=%d candidate_claim_grace=%s\n",
			len(report.StaleLocks), len(report.ExpiredClaims), len(report.StaleCandidateClaims), len(residue), domain.CandidateClaimReclaimGrace)
		if scanIncomplete {
			return 3
		}
		return 0
	}
	if *repair {
		if code := doRepair(rt, live, *restoreOrphan, orphans, residue, stdout, stderr); code != 0 {
			return code
		}
	}
	if scanIncomplete {
		return 3
	}
	return 0
}

// printDoctorHeader writes doctor's environment-identity lines (project/repo/
// state) plus the guard-reachability rows (loto-jomg): both answer "is my
// environment sound" ahead of the audit findings below, and folding them into
// one call keeps cmdDoctor under funlen's statement budget.
func printDoctorHeader(ctx context.Context, stdout io.Writer, repoTop, stateDir string) {
	fmt.Fprintf(stdout, "project: %s\n", ResolveAndPinProjectSlug(repoTop))
	fmt.Fprintf(stdout, "repo:    %s\n", repoTop)
	fmt.Fprintf(stdout, "state:   %s\n", stateDir)
	renderGuardReachability(stdout, checkGuardReachability(ctx, repoTop))
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
func scanOrphansAndHint(rt *runtime, repoTop string, live domain.HolderLiveProbe, f orphanFlags, stdout io.Writer) (orphans []string, scanIncomplete bool) {
	if !f.orphanMode && !f.restoreOrphan {
		return nil, false
	}
	orphans, scanIncomplete = runOrphanScan(rt, repoTop, live, stdout)
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

func doRepair(rt *runtime, live domain.HolderLiveProbe, restoreOrphan bool, orphans []string, residue []claimResidue, stdout, stderr io.Writer) int {
	if err := rt.Store.DoctorRepair(rt.Ctx, domain.AgentUUID(rt.Agent.UUID), live); err != nil {
		fmt.Fprintf(stderr, "✗ repair: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, "✓ repaired")
	if code := releaseClaimResidue(rt, residue, stdout, stderr); code != 0 {
		return code
	}
	if restoreOrphan && len(orphans) > 0 {
		restored, failures, err := rt.Store.RestoreOrphanMode(rt.Ctx, live, orphans)
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
func runOrphanScan(rt *runtime, repoTop string, live domain.HolderLiveProbe, stdout io.Writer) ([]string, bool) {
	scan, skipped, firstErr := scanAndReportOrphans(rt, repoTop, live, stdout)
	if skipped > 0 {
		fmt.Fprintf(stdout, "✗ scan-skipped count=%d first=%v\n", skipped, firstErr)
		return scan, true
	}
	return scan, false
}

// scanAndReportOrphans walks the repo for orphan-mode candidates. It returns
// the orphan list, a count of walk entries skipped due to errors (e.g.
// permission-denied subtrees), and the first walk error encountered. Callers
// must surface a non-zero skipped count as an incomplete-scan signal; silently
// dropping these would produce a false-clean report (gh#130).
// ‡ Candidates are ABSOLUTE, joined from repoTop by repoCandidates, and the
// store reconciles them against repo-relative lock rows using ITS OWN root
// (WithRepoTop, loto-6e02). The root is a property of the store's data, not of
// the call — passing it per-call is how the two got out of step in loto-qoic.
func scanAndReportOrphans(rt *runtime, repoTop string, live domain.HolderLiveProbe, stdout io.Writer) ([]string, int, error) {
	candidates, skipped, firstWalkErr := repoCandidates(rt.Ctx, repoTop)
	orphans, err := rt.Store.ScanOrphanModes(rt.Ctx, live, candidates)
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

// errScanIncomplete wraps the warning git wrote when it could not read part of
// the tree. Static so callers can errors.Is it; the warning text rides along.
var errScanIncomplete = errors.New("orphan scan incomplete")

// walkSkipSegments is the belt, not the mechanism. git's --exclude-standard is
// what actually bounds the candidate set (repoCandidates); this list is what
// stands between a mis-configured repo — no .gitignore, a broken git — and a
// mass chmod of a dependency cache. Matched on any repo-relative segment.
var walkSkipSegments = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".cache": true,
	".beads": true,
}

// walkSkipPrefixes are repo-relative path prefixes excluded outright. A
// repo-local GOMODCACHE is 0444 by contract, and restoring write on it is the
// destructive false positive loto-3we5 was opened about.
var walkSkipPrefixes = []string{".sandbox/cache/"}

// repoCandidates enumerates the files an orphan-mode scan may consider:
// everything git tracks or would track, which is exactly everything that is
// not ignored.
//
// ‡ git is the source, not a filter over a walk. --exclude-standard is the
// only thing that honors all three ignore inputs at once — per-directory
// .gitignore, $GIT_DIR/info/exclude, and core.excludesFile — and on this repo
// the difference is decisive: .sandbox/cache/ is in .gitignore, but .beads/ is
// ignored ONLY via .git/info/exclude. A hand-parsed .gitignore would still walk
// the entire dolt object store and hand --restore-orphan-mode 19 packfiles.
//
// The old whole-tree WalkDir touched 33,236 files here in 6.75s and reported
// 9,306 orphans, of which 9,287 were read-only-by-design cache entries.
//
// skipped/firstErr keep the gh#130 incomplete-scan contract: a scan that could
// not see everything must never read as clean. git failing entirely is one
// error and zero candidates — never a silent empty set.
func repoCandidates(ctx context.Context, root string) (out []string, skipped int, firstErr error) {
	if root == "" {
		return nil, 0, nil
	}
	raw, warnings, err := gitLsFiles(ctx, root)
	if err != nil {
		return nil, 1, fmt.Errorf("git ls-files in %s: %w", root, err)
	}
	// git exits 0 on an unreadable subtree and merely warns, so the warnings are
	// the whole incomplete-scan signal here (gh#130). Dropping them would let a
	// permission-denied directory read as a clean scan — the failure mode the
	// WalkDir error callback existed to prevent.
	for _, w := range warnings {
		skipped++
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: %s", errScanIncomplete, w)
		}
	}
	for rel := range strings.SplitSeq(raw, "\x00") {
		abs, cerr, ok := candidatePath(root, rel)
		if cerr != nil {
			skipped++
			if firstErr == nil {
				firstErr = cerr
			}
			continue
		}
		if ok {
			out = append(out, abs)
		}
	}
	return out, skipped, firstErr
}

// gitLsFiles runs the candidate enumeration and returns stdout, the warning
// lines git wrote to stderr, and a hard error. Separate from gitCmd because
// only this caller needs stderr on SUCCESS: git reports an unreadable directory
// as a warning and exit 0.
func gitLsFiles(ctx context.Context, root string) (stdout string, warnings []string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if runErr := cmd.Run(); runErr != nil {
		return "", nil, runErr
	}
	for line := range strings.SplitSeq(strings.TrimSpace(errBuf.String()), "\n") {
		if line != "" {
			warnings = append(warnings, line)
		}
	}
	return outBuf.String(), warnings, nil
}

// candidatePath decides one git-listed entry: the absolute path and true when
// it is a scannable regular file, a non-nil error when the entry exists but
// could not be read (an incomplete-scan signal), false otherwise.
//
// ‡ git lists symlinks, and paths it saw before they were deleted. The scan is
// about file modes, so neither is a candidate. WalkDir gave this for free.
func candidatePath(root, rel string) (string, error, bool) { //nolint:revive // (path, err, ok) reads better here than a struct for one caller
	if rel == "" || skipCandidate(rel) {
		return "", nil, false
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, false // listed but gone: not an incomplete scan
		}
		return "", fmt.Errorf("%s: %w", rel, err), false
	}
	if !fi.Mode().IsRegular() {
		return "", nil, false
	}
	return abs, nil, true
}

// skipCandidate applies the belt to one repo-relative POSIX path.
func skipCandidate(rel string) bool {
	for _, pre := range walkSkipPrefixes {
		if strings.HasPrefix(rel, pre) {
			return true
		}
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if walkSkipSegments[seg] {
			return true
		}
	}
	return false
}

// claimResidue is one candidate whose durable claims outlived an acceptance
// that never finished writing its refs.
type claimResidue struct {
	CandidateID string
	Paths       []string
}

// scanClaimResidue finds candidate claims whose candidate ref is absent —
// acceptance residue (loto-ovno.12, Codex #261 P1).
//
// AcceptCandidate inserts the claims, then writes refs/loto/candidates/<id>
// last. Its in-process failure paths compensate, but a SIGKILL or power loss
// between the two cannot run anything: the claims survive with no ref behind
// them, acquisition reads every claim as unresolved, and the write set is
// blocked with no way to clear it. Because the refs are written last, their
// absence is decisive — a claim whose candidate id has no ref is residue.
//
// ‡ A failed ref read returns an error and NO residue. Reading "git failed" as
// "no refs exist" would classify every live candidate's claims as residue and
// hand --repair a delete list covering the whole store.
func scanClaimResidue(rt *runtime, repoTop string) ([]claimResidue, error) {
	claims, err := rt.Store.ListCandidateClaims(rt.Ctx)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, nil
	}
	if repoTop == "" {
		return nil, errNoRepoForResidue
	}
	ids, err := gate.ListCandidateIDs(rt.Ctx, repoTop)
	if err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		live[id] = struct{}{}
	}
	byID := make(map[string][]string)
	for i := range claims {
		if _, ok := live[claims[i].CandidateID]; ok {
			continue
		}
		byID[claims[i].CandidateID] = append(byID[claims[i].CandidateID], claims[i].PathCanonical)
	}
	out := make([]claimResidue, 0, len(byID))
	for id, paths := range byID {
		sort.Strings(paths)
		out = append(out, claimResidue{CandidateID: id, Paths: paths})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CandidateID < out[j].CandidateID })
	return out, nil
}

// errNoRepoForResidue: without a repo top there is no candidates namespace to
// read, so residue cannot be told from a live candidate. Reported, never
// silently treated as "clean" — see scanClaimResidue's second ‡.
var errNoRepoForResidue = errors.New("loto: cannot check candidate refs outside a repo")

// reportClaimResidue renders the residue rows and returns them. A read failure
// is a ⚠ advisory, not a hard error: doctor's other checks still ran, and the
// repair path below refuses to act on an unknown state anyway.
func reportClaimResidue(rt *runtime, repoTop string, stdout io.Writer) []claimResidue {
	residue, err := scanClaimResidue(rt, repoTop)
	if err != nil {
		fmt.Fprintf(stdout, "⚠ claim-residue-scan err=%v\n", err)
		return nil
	}
	for _, r := range residue {
		fmt.Fprintf(stdout, "⚠ claim-residue candidate=%s paths=%d first=%s\n",
			r.CandidateID, len(r.Paths), r.Paths[0])
	}
	if len(residue) > 0 {
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintln(stdout, "loto doctor --repair")
		fmt.Fprintln(stdout, "```")
	}
	return residue
}

// releaseClaimResidue drops the residue rows under --repair. Each candidate is
// released on its own so one failure cannot hide the rest.
func releaseClaimResidue(rt *runtime, residue []claimResidue, stdout, stderr io.Writer) int {
	released := 0
	for _, r := range residue {
		if err := rt.Store.ReleaseCandidateClaims(rt.Ctx, r.CandidateID); err != nil {
			fmt.Fprintf(stderr, "✗ claim-residue candidate=%s err=%v\n", r.CandidateID, err)
			return 3
		}
		released += len(r.Paths)
	}
	if released > 0 {
		fmt.Fprintf(stdout, "✓ claim-residue-released candidates=%d paths=%d\n", len(residue), released)
	}
	return 0
}

// --- guard reachability (loto-jomg) -----------------------------------------
//
// Reachability has ONE definition now that loto-ea8y.1 (PR #318) landed the
// hooks.d chain-runner: core.hooksPath resolves to the repo's tracked
// .githooks, the dispatcher for the hook exists and is executable, and a
// loto-owned entry exists and is executable under .githooks/hooks.d/<hook>/.
// No PATH probe, no alias probe — that delivery is gone (decision e10cdeda65c9).

// guardSpec names one guard's git hook and the label doctor/status print for it.
type guardSpec struct {
	hook  string // git hook name: the dispatcher + hooks.d/<hook> chain dir
	label string // printed as guard=<label>
}

// guardSpecs is the fixed, ordered set of guards this bead reports on. Order
// is print order, so it is deterministic on its own — no sort needed.
var guardSpecs = []guardSpec{
	{hook: "pre-commit", label: "pre-commit-gate"},
	{hook: "post-checkout", label: "tree-move-guard"},
}

// guardStatus is one guard's reachability verdict. reason and detail are only
// meaningful when !OK.
type guardStatus struct {
	spec   guardSpec
	ok     bool
	reason string // machine-stable token, e.g. "hooksPath-foreign", "missing-entry"
	detail string // the value or path that names the specific problem
}

// checkGuardReachability evaluates every guard in guardSpecs against the one
// definition above. The rows are independent by construction: a hooksPath
// problem fails every guard (nothing can fire without it), but a per-guard
// dispatcher/entry problem fails only that guard.
func checkGuardReachability(ctx context.Context, repoTop string) []guardStatus {
	out := make([]guardStatus, len(guardSpecs))
	if repoTop == "" {
		for i, spec := range guardSpecs {
			out[i] = guardStatus{spec: spec, reason: "no-repo", detail: "not inside a git repository"}
		}
		return out
	}
	resolved, hooksPathOK, err := resolveGitHooksPath(ctx, repoTop)
	for i, spec := range guardSpecs {
		switch {
		case err != nil:
			out[i] = guardStatus{spec: spec, reason: "hooksPath-unreadable", detail: err.Error()}
		case !hooksPathOK:
			out[i] = guardStatus{spec: spec, reason: "hooksPath-foreign", detail: resolved}
		default:
			out[i] = checkOneGuard(repoTop, spec)
		}
	}
	return out
}

// resolveGitHooksPath resolves core.hooksPath (or git's unset-default,
// $GIT_DIR/hooks) to an absolute path and reports whether it matches the
// repo's tracked .githooks. Mirrors the comparison `make hooks` makes, but
// resolves to an absolute path so a foreign value can be named in full
// (make hooks compares the raw config string only).
//
// The `git config --get` call below carries no --local/--global flag, so git
// itself resolves local > global > system and returns the EFFECTIVE value —
// local unset falls through to a foreign global rather than reading as unset.
// loto-p6zs measured this machine's actual shape (local unset, foreign
// global, every repo) and TestDoctorGuard_InheritedGlobalHooksPath pins it: a
// prior hypothesis that this check tested the local override rather than the
// effective path was wrong — the blind spot dk saw was the installed loto
// binary predating this feature (PR #322), not this predicate.
func resolveGitHooksPath(ctx context.Context, repoTop string) (resolved string, ok bool, err error) {
	want := filepath.Clean(filepath.Join(repoTop, ".githooks"))

	raw, cerr := gitCmd(ctx, repoTop, "config", "--get", "core.hooksPath")
	raw = strings.TrimSpace(raw)
	if cerr != nil {
		var exitErr *exec.ExitError
		if !errors.As(cerr, &exitErr) || exitErr.ExitCode() != 1 {
			return "", false, fmt.Errorf("git config --get core.hooksPath: %w", cerr)
		}
		raw = "" // exit 1: key unset, not an error
	}

	if raw == "" {
		gitDirRaw, derr := gitCmd(ctx, repoTop, "rev-parse", "--git-dir")
		if derr != nil {
			return "", false, fmt.Errorf("git rev-parse --git-dir: %w", derr)
		}
		gitDir := strings.TrimSpace(gitDirRaw)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repoTop, gitDir)
		}
		resolved = filepath.Clean(filepath.Join(gitDir, "hooks"))
		return resolved, resolved == want, nil
	}

	if filepath.IsAbs(raw) {
		resolved = filepath.Clean(raw)
	} else {
		resolved = filepath.Clean(filepath.Join(repoTop, raw))
	}
	return resolved, resolved == want, nil
}

// checkOneGuard runs the dispatcher + hooks.d entry half of the definition for
// one guard, given that core.hooksPath already resolves to .githooks.
func checkOneGuard(repoTop string, spec guardSpec) guardStatus {
	dispatcherRel := ".githooks/" + spec.hook
	fi, err := os.Stat(filepath.Join(repoTop, filepath.FromSlash(dispatcherRel)))
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return guardStatus{spec: spec, reason: "missing-dispatcher", detail: dispatcherRel}
	}

	chainDirRel := ".githooks/hooks.d/" + spec.hook
	entries, err := os.ReadDir(filepath.Join(repoTop, filepath.FromSlash(chainDirRel)))
	if err != nil {
		return guardStatus{spec: spec, reason: "missing-entry", detail: chainDirRel}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !isScannableHookEntry(name) || !strings.Contains(name, "loto") {
			continue
		}
		entryRel := chainDirRel + "/" + name
		info, statErr := os.Stat(filepath.Join(repoTop, filepath.FromSlash(entryRel)))
		if statErr != nil || info.Mode()&0o111 == 0 {
			return guardStatus{spec: spec, reason: "not-executable", detail: entryRel}
		}
		return guardStatus{spec: spec, ok: true, detail: entryRel}
	}
	return guardStatus{spec: spec, reason: "missing-entry", detail: chainDirRel}
}

// isScannableHookEntry mirrors run-chain.sh's own skip rules (its header,
// .githooks/lib/run-chain.sh): *.orig, *.sample, *.disabled and dotfiles are
// never run, so they must never count as the loto entry either.
func isScannableHookEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	switch filepath.Ext(name) {
	case ".orig", ".sample", ".disabled":
		return false
	}
	return true
}

// guardsAllReachable reduces a reachability report to the single bool status
// prints as guard=ok / guard=inert.
func guardsAllReachable(statuses []guardStatus) bool {
	for _, s := range statuses {
		if !s.ok {
			return false
		}
	}
	return true
}

// guardSummary renders checkGuardReachability's verdict for status's one-line
// header: "ok" only when every guard is reachable, "inert" otherwise — a
// guard that cannot prove it is installed is reported as absent (loto-jomg).
func guardSummary(statuses []guardStatus) string {
	if guardsAllReachable(statuses) {
		return "ok"
	}
	return "inert"
}

// renderGuardReachability prints one row per guard and, when any is
// unreachable, one shared fix block (design.md: a ```bash fix block under a ✗
// row). Returns whether every guard is reachable, for callers that also need
// the bool (status's guard=ok/guard=inert line).
func renderGuardReachability(stdout io.Writer, statuses []guardStatus) bool {
	anyFail := false
	for _, s := range statuses {
		if s.ok {
			fmt.Fprintf(stdout, "✓ guard=%s reachable entry=%s\n", s.spec.label, s.detail)
			continue
		}
		anyFail = true
		fmt.Fprintf(stdout, "✗ guard=%s unreachable reason=%s detail=%s\n", s.spec.label, s.reason, s.detail)
	}
	if anyFail {
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintln(stdout, "make hooks")
		fmt.Fprintln(stdout, "```")
	}
	return !anyFail
}
