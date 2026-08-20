package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

const (
	tcFlagDryRun        = "--dry-run"
	tcFlagRestoreOrphan = "--restore-orphan-mode"
)

func TestDoctorHealthyEmpty(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "healthy") {
		t.Errorf("expected ✓ healthy: %q", out.String())
	}
}

func TestDoctorDryRunDoesNotMutate(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagDryRun}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor --dry-run exit %d", code)
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected dry-run line: %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"status", tcFlagMine}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatal("status failed")
	}
	if !strings.Contains(out.String(), "target=a.go") {
		t.Errorf("lock should still exist after --dry-run; got %q", out.String())
	}
}

func TestDoctor_OrphanModeFlaggedNotRepaired(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagRepair, tcFlagOrphan}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}

	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("orphan unexpectedly restored: %o", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("expected orphan-mode in output: %s", out.String())
	}
}

// TestDoctor_OrphanModeSurfacesRecoveryHint verifies that when orphan-mode files
// are found, the output points the user at the recovery command. The orphan-mode
// state is otherwise a dead-end: the file is read-only with no lock row, and a
// user who does not already know about --restore-orphan-mode cannot recover it
// (e.g. a SIGKILL between strip and commit in lock acquire, loto-j863).
func TestDoctor_OrphanModeSurfacesRecoveryHint(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "orphan-mode target=orphan.go") {
		t.Fatalf("expected orphan finding in output: %s", got)
	}
	if !strings.Contains(got, tcFlagRestoreOrphan) {
		t.Errorf("orphan-mode output must point at recovery command --restore-orphan-mode: %s", got)
	}
}

func TestDoctor_NoOrphanHintWhenClean(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), tcFlagRestoreOrphan) {
		t.Errorf("clean scan must not emit recovery hint: %s", out.String())
	}
}

func TestDoctor_DefaultDoesNotWalkTree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("default doctor should not walk: %s", out.String())
	}
}

// TestDoctor_OrphanModeWalkErrorIsSurfaced verifies that filepath.WalkDir errors
// (e.g. permission-denied subtrees) are surfaced rather than silently swallowed.
// Without the fix, the doctor reports a clean scan even when files were inaccessible.
func TestDoctor_OrphanModeWalkErrorIsSurfaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not gate root")
	}
	repo := withTempProject(t)
	pinAgent(t)

	// Create an unreadable subdir: WalkDir will invoke the fn with a non-nil err
	// when attempting to read its children.
	denied := filepath.Join(repo, "denied")
	if err := os.Mkdir(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "inside.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard)
	got := out.String()

	if code == 0 {
		t.Errorf("expected non-zero exit on incomplete scan, got 0: %s", got)
	}
	if !strings.Contains(got, "scan-skipped") {
		t.Errorf("expected ✗ scan-skipped line in output: %s", got)
	}
	if !strings.Contains(got, "✗") {
		t.Errorf("expected ✗ glyph: %s", got)
	}
}

func TestDoctor_RestoreOrphanModeFlagRepairs(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagRepair, tcFlagRestoreOrphan}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}

	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored, got %o", st.Mode().Perm())
	}
}

// TestDoctor_ExpiredClaims_ListedAndRepaired covers D3's CLI surface
// (loto-ebkc): doctor lists TTL-lapsed claims as findings, --repair sweeps
// them (gcClaimsTx), and the next audit is healthy again.
func TestDoctor_ExpiredClaims_ListedAndRepaired(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	// The claim verb rejects non-positive TTLs: shortest lease + wait out expiry.
	if code := Run([]string{"claim", "internal/store", "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, io.Discard, io.Discard); code != 0 {
		t.Fatal("claim failed")
	}
	time.Sleep(20 * time.Millisecond)

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "expired_claims=1") {
		t.Errorf("triage line must count expired claims: %q", got)
	}
	if !strings.Contains(got, "expired_claim prefix=internal/store owner=") {
		t.Errorf("expected expired-claim row: %q", got)
	}

	// Dry-run names the claims sweep too, not just lock reclaims.
	out.Reset()
	if code := Run([]string{tcCmdDoctor, tcFlagDryRun}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor --dry-run exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "would_gc_claims=1") {
		t.Errorf("dry-run must report would_gc_claims: %q", out.String())
	}

	out.Reset()
	if code := Run([]string{tcCmdDoctor, tcFlagRepair}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor --repair exit %d: %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{tcCmdDoctor}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor after repair exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ healthy") {
		t.Errorf("expired claim must be swept by --repair: %q", out.String())
	}
}

// --- acceptance residue (loto-ovno.12) --------------------------------------

// seedClaim reproduces the state a killed acceptance leaves behind: take a
// real lease, convert it to a durable claim through the same guarded store
// call AcceptCandidate uses — and stop there, writing no refs. That is exactly
// the window loto-ovno.12 closes.
func seedClaim(t *testing.T, candidateID, path string) {
	t.Helper()
	if code := Run([]string{tcCmdLock, path, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Store.Close()
	target, err := resolveCLITarget(callerBase(), rt.RepoTop, path)
	if err != nil {
		t.Fatal(err)
	}
	owner := domain.AgentUUID(rt.Agent.UUID)
	locks, err := rt.Store.LocksForOwnerAt(rt.Ctx, []domain.Target{target}, owner)
	if err != nil {
		t.Fatal(err)
	}
	held, ok := locks[target.Canonical]
	if !ok {
		t.Fatalf("no lease on %s after lock", target.Canonical)
	}
	claims := []domain.CandidateClaim{{
		PathCanonical: target.Canonical, CandidateID: candidateID,
		OwnerUUID: owner, SessionUUID: rt.SessionUUID,
		CreatedAt: time.Now(), Host: rt.Host, PID: 1,
	}}
	guard := store.ClaimGuard{
		Owner: owner,
		Epoch: map[string]int64{target.Canonical: held.Epoch},
		Live:  rt.liveProbe(),
	}
	if err := rt.Store.InsertCandidateClaims(rt.Ctx, claims, guard); err != nil {
		t.Fatal(err)
	}
}

func claimPaths(t *testing.T) []string {
	t.Helper()
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Store.Close()
	claims, err := rt.Store.ListCandidateClaims(rt.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(claims))
	for i := range claims {
		out[i] = claims[i].PathCanonical
	}
	return out
}

func TestDoctorReportsClaimResidue(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	seedClaim(t, "cand-dead", tcTargetA)

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "claim-residue candidate=cand-dead") {
		t.Errorf("missing residue row: %q", out.String())
	}
	// Reporting alone must not delete anything.
	if got := claimPaths(t); len(got) != 1 {
		t.Errorf("doctor without --repair must not release, got %v", got)
	}
}

func TestDoctorRepairReleasesClaimResidue(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	seedClaim(t, "cand-dead", tcTargetA)

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagRepair}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor --repair exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "claim-residue-released candidates=1 paths=1") {
		t.Errorf("missing release line: %q", out.String())
	}
	if got := claimPaths(t); len(got) != 0 {
		t.Errorf("residue survived --repair: %v", got)
	}
}

// The decisive half: a claim whose candidate ref EXISTS is a live candidate
// under review, and --repair must leave it alone. Without this the repair pass
// would clear the very protection acceptance just established.
func TestDoctorRepairKeepsClaimsWithLiveCandidateRef(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	seedClaim(t, "cand-live", tcTargetA)
	// A candidates ref may point at any object; doctor only asks whether the
	// ref exists, matching AcceptCandidate's write-refs-last ordering.
	if err := os.WriteFile(filepath.Join(repo, "envelope.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := submitGitT(t, repo, "hash-object", "-w", "envelope.json")
	submitGitT(t, repo, "update-ref", "refs/loto/candidates/cand-live", blob)

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagRepair}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor --repair exit %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "claim-residue") {
		t.Errorf("a claim with a live candidate ref is not residue: %q", out.String())
	}
	if got := claimPaths(t); len(got) != 1 {
		t.Errorf("live candidate's claims must survive --repair, got %v", got)
	}
}

func TestDoctorDryRunCountsResidueWithoutReleasing(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	seedClaim(t, "cand-dead", tcTargetA)

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagDryRun}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor --dry-run exit %d", code)
	}
	if !strings.Contains(out.String(), "would_release_residue=1") {
		t.Errorf("missing dry-run count: %q", out.String())
	}
	if got := claimPaths(t); len(got) != 1 {
		t.Errorf("dry-run must not release, got %v", got)
	}
}

// TestDoctor_OrphanModeSkipsLiveLockedFile pins the loto-qoic contract: a file
// holding a live lock row is never reported as orphan-mode residue, whatever
// path form the CLI hands the store. Pre-fix the CLI supplies absolute
// candidates while locks.target_canonical is repo-relative, so the owned-lock
// filter in ScanOrphanModes never matches and a.go is reported.
func TestDoctor_OrphanModeSkipsLiveLockedFile(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	// The chmod is this test's own act, not something `lock` produces: loto no
	// longer strips the write bit on acquire (loto-zssw). Do not "simplify" this
	// away on the theory that locking makes the file read-only.
	if err := os.Chmod(filepath.Join(repo, tcTargetA), 0o444); err != nil {
		t.Fatal(err)
	}
	// Control. Read-only with no lock row, so it must still be reported. Without
	// it this test passes trivially whenever the scan returns nothing at all.
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor %s exit %d: %s", tcFlagOrphan, code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "orphan-mode target="+tcTargetB) {
		t.Errorf("genuine orphan %s must still be reported: %q", tcTargetB, got)
	}
	if strings.Contains(got, "orphan-mode target="+tcTargetA) {
		t.Errorf("%s holds a live lock and must not be reported as an orphan: %q", tcTargetA, got)
	}
	fi, err := os.Stat(filepath.Join(repo, tcTargetA))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("a scan without %s must not chmod: mode=%04o", tcFlagRepair, fi.Mode().Perm())
	}
}

// TestDoctor_RestoreOrphanModeSkipsLiveLockedFile pins the restore half: a
// live-locked file never enters the orphan list, so --restore-orphan-mode
// cannot chmod it. Pre-fix the restore count reads 2 rather than 1.
func TestDoctor_RestoreOrphanModeSkipsLiveLockedFile(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	if err := os.Chmod(filepath.Join(repo, tcTargetA), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagRepair, tcFlagRestoreOrphan}, &out, io.Discard); code != 0 {
		t.Fatalf("doctor %s --restore-orphan-mode exit %d: %s", tcFlagRepair, code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "restored-orphan-mode count=1 failed=0") {
		t.Errorf("only the genuine orphan should be restored: %q", got)
	}
	// The count is arity, not identity — doRepair prints no per-path line for a
	// successful restore — so pin which file was in the list.
	if n := strings.Count(got, "orphan-mode target="); n != 1 {
		t.Errorf("expected exactly one orphan-mode row, got %d: %q", n, got)
	}
	if strings.Contains(got, "orphan-mode target="+tcTargetA) {
		t.Errorf("%s holds a live lock and must not enter the orphan list: %q", tcTargetA, got)
	}
	fi, err := os.Stat(filepath.Join(repo, tcTargetB))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("genuine orphan %s should have been restored: mode=%04o", tcTargetB, fi.Mode().Perm())
	}
	// Deliberately NOT asserting a.go's mode. DoctorRepair's chmod-era migration
	// (chmodEraCandidates -> restoreChmodEraFiles) re-adds owner-write to every
	// current lock row's path by design, so a.go's mode after --repair says
	// nothing about the orphan path. That harm is tracked as loto-2hjh. What this
	// test pins is that a.go never enters the orphan list at all.
}

// TestDoctor_OrphanScanExcludesIgnoredTrees is the loto-3we5 regression. The
// whole-tree walk reported 9,316 orphans in this repo, 9,306 of them files that
// are read-only BY CONTRACT: the Go module cache and a dolt repo's git objects.
// `--repair --restore-orphan-mode` would have chmod +w'd every one. Two
// exclusion routes are pinned here because only their union covers this repo:
// .gitignore, and $GIT_DIR/info/exclude (which is how .beads/ is ignored).
func TestDoctor_OrphanScanExcludesIgnoredTrees(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	mkRO := func(rel string) string {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	excludeFile := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludeFile, []byte("dolt/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gitignored := mkRO("cache/mod/dep@v1/go.mod")
	infoExcluded := mkRO("dolt/repo.git/objects/pack/x.pack")
	beltOnly := mkRO(".sandbox/cache/mod/dep@v2/go.mod")
	genuine := mkRO("orphan.go")

	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	got := out.String()

	if !strings.Contains(got, "orphan-mode target=orphan.go") {
		t.Errorf("genuine residue in a tracked path must still be reported: %s", got)
	}
	for name, p := range map[string]string{
		"gitignored":    gitignored,
		"info/exclude":  infoExcluded,
		"belt (prefix)": beltOnly,
	} {
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, rel) {
			t.Errorf("%s tree must not be scanned (%s): %s", name, rel, got)
		}
	}

	// The destructive half: --restore-orphan-mode must leave every excluded
	// file at 0444. This is the assertion the bead is actually about.
	if code := Run([]string{tcCmdDoctor, tcFlagRepair, "--restore-orphan-mode"}, &bytes.Buffer{}, io.Discard); code != 0 {
		t.Fatalf("repair exit %d", code)
	}
	for _, p := range []string{gitignored, infoExcluded, beltOnly} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o444 {
			t.Errorf("%s: read-only-by-design file was chmod'd to %o", p, st.Mode().Perm())
		}
	}
	st, err := os.Stat(genuine)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("genuine residue must still be restorable, perm=%o", st.Mode().Perm())
	}
}
