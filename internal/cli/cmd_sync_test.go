package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

const (
	tcCmdSync = "sync"
)

func syncGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// syncBaseRepo commits the withTempProject fixture and plants
// refs/loto/integration at HEAD — the shared setup every sync test starts
// from (φ TestSubmit_HappyPath's own base-commit + pin convention).
func syncBaseRepo(t *testing.T) string {
	t.Helper()
	repo := withTempProject(t)
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "base")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	pinAgent(t)
	return repo
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- Step 1: skeleton red — integration absent / clean tree ---

// TestSync_IntegrationAbsent pins the anti-bootstrap contract: sync must
// never call gate.ResolveIntegrationRef (it mints the ref as a side effect).
// A fresh repo with no refs/loto/integration gets the neutral empty-status
// header, exit 0, and the ref must still not exist afterward.
func TestSync_IntegrationAbsent(t *testing.T) {
	repo := withTempProject(t)
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if out.String() != "ℹ sync synced=0 conflicts=0 integration=absent\n" {
		t.Errorf("unexpected header: %q", out.String())
	}
	// rev-parse --verify --quiet exits non-zero on a missing ref by design —
	// don't use syncGitT (fatals on any git error) to check for absence.
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/loto/integration")
	cmd.Dir = repo
	if out, err := cmd.Output(); err == nil {
		t.Errorf("sync must never bootstrap the integration ref, got %q", strings.TrimSpace(string(out)))
	}
}

// TestSync_CleanTreeNoOp: a tree that already matches integration is a no-op,
// with an explicit empty-status header (design.md: silence looks like a crash).
func TestSync_CleanTreeNoOp(t *testing.T) {
	syncBaseRepo(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	want := "✓ sync synced=0 conflicts=0 tree=matches-integration\n"
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

// --- Step 2: fast-forward red ---

// TestSync_FastForwardsUnleasedDivergentPath: an unleased path whose content
// has drifted from integration is restored, and nothing else in the tree is
// touched.
func TestSync_FastForwardsUnleasedDivergentPath(t *testing.T) {
	repo := syncBaseRepo(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=1 conflicts=0 skipped=0") {
		t.Errorf("missing triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ target="+tcTargetA+" action=fast-forward") {
		t.Errorf("missing success row: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("content not restored, got %q", got)
	}
	if status := syncGitT(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("git status must be clean after sync restores to integration==HEAD, got %q", status)
	}
}

// TestSync_RestoresDeletedPath: a path removed from disk, directory and all,
// is recreated (exercises MkdirAll + the missing-state branch).
func TestSync_RestoresDeletedPath(t *testing.T) {
	repo := syncBaseRepo(t)
	if err := os.RemoveAll(filepath.Join(repo, tcPrefixParent, "store")); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ target="+tcStoreStoreGo+" action=fast-forward") {
		t.Errorf("missing success row: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(repo, tcStoreStoreGo)); err != nil {
		t.Errorf("path not restored: %v", err)
	}
}

// --- Step 3: conflict red ---

// TestSync_RefusesLeasedPath: a divergent path under a live lease is refused
// and left untouched.
func TestSync_RefusesLeasedPath(t *testing.T) {
	repo := syncBaseRepo(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✗ target="+tcTargetA+" reason=leased holder=") {
		t.Errorf("missing conflict row: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "junk\n" {
		t.Errorf("leased content must be untouched, got %q", got)
	}
}

// TestSync_RefusesCandidateClaimedPath: a path with a durable candidate claim
// (minted by a full submit flow, φ TestSubmit_HappyPath) is refused even
// after its originating lease is released.
func TestSync_RefusesCandidateClaimedPath(t *testing.T) {
	repo := syncBaseRepo(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, io.Discard, io.Discard); code != 0 {
		t.Fatal("submit")
	}
	// AcceptCandidate does not release the original lease (TestSubmit_HappyPath) —
	// release it explicitly so this test isolates the candidate-claim conflict
	// from the leased conflict (syncDecide checks leases first).
	if code := Run([]string{tcCmdUnlock, tcTargetA, "-t", tcIntentDone}, io.Discard, io.Discard); code != 0 {
		t.Fatal("unlock")
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✗ target="+tcTargetA+" reason=candidate-claim candidate=c-") {
		t.Errorf("missing conflict row: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "edited\n" {
		t.Errorf("candidate-claimed content must be untouched, got %q", got)
	}
}

// TestSync_RefusesBehindHeadIntegration is plan-review P1-2: refs/loto/integration
// existing as a strict ancestor of HEAD refuses the whole run before any write.
func TestSync_RefusesBehindHeadIntegration(t *testing.T) {
	repo := syncBaseRepo(t)
	integSHA := syncGitT(t, repo, "rev-parse", "refs/loto/integration")

	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "advance main past integration")
	headSHA := syncGitT(t, repo, "rev-parse", "HEAD")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	want := "✗ sync synced=0 conflicts=0 integration=behind-HEAD\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Errorf("got %q want prefix %q", out.String(), want)
	}
	if !strings.Contains(out.String(), "ℹ integration="+integSHA+" head="+headSHA) {
		t.Errorf("missing SHA row: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("refusal must write nothing, a.go changed: %q", got)
	}
}

// --- Step 4: report/exit-code red ---

// TestSync_MixedReport: one syncable path and one leased divergent path in
// the same run — exit 1, both counted, rows path-sorted, byte-exact.
func TestSync_MixedReport(t *testing.T) {
	repo := withTempProject(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "base")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("junk-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("want at least 3 lines, got %d: %q", len(lines), out.String())
	}
	if lines[0] != "✗ sync synced=1 conflicts=1 skipped=0" {
		t.Errorf("bad triage line: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "✓ target="+tcTargetA+" action=fast-forward") {
		t.Errorf("want a.go row before b.go (path-sorted), got: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "✗ target="+tcTargetB+" reason=leased holder=") {
		t.Errorf("want b.go conflict row second, got: %q", lines[2])
	}
	if !strings.Contains(out.String(), "loto status --collisions") {
		t.Errorf("missing fix block: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("a.go not restored: %q", got)
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetB)); got != "junk-b\n" {
		t.Errorf("b.go must stay untouched: %q", got)
	}
}

// TestSync_Idempotent: a second run after a clean sync finds no divergence.
func TestSync_Idempotent(t *testing.T) {
	repo := syncBaseRepo(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdSync}, io.Discard, io.Discard); code != 0 {
		t.Fatal("first sync")
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	want := "✓ sync synced=0 conflicts=0 tree=matches-integration\n"
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

// TestSync_RejectsStrayArgs: sync takes no arguments in v1.
func TestSync_RejectsStrayArgs(t *testing.T) {
	syncBaseRepo(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync, "extra"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "usage: loto sync") {
		t.Errorf("missing usage head: %q", errBuf.String())
	}
}

// --- syncDecide: pure unit table (plan step 3) ---

func TestSyncDecide_ConflictTable(t *testing.T) {
	now := time.Now()
	const (
		path      = "internal/a.go"
		syncOwner = "owner-1"
	)
	diffs := []syncDiff{{syncEntry: syncEntry{Path: path, Mode: "100644", OID: "deadbeef"}, State: syncModified}}
	ec := domain.EvalContext{Now: now}

	tests := []struct {
		name       string
		locks      []domain.LockRecord
		claims     []domain.ClaimRecord
		cands      []domain.CandidateClaim
		wantApply  bool
		wantReason string
	}{
		{
			name:      "no coverage -> apply",
			wantApply: true,
		},
		{
			name: "live lease any owner -> conflict",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: path}, OwnerUUID: syncOwner, ExpiresAt: now.Add(time.Hour)},
			},
			wantReason: syncReasonLeased,
		},
		{
			name: "stale lease -> syncable",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: path}, OwnerUUID: syncOwner, ExpiresAt: now.Add(-time.Hour)},
			},
			wantApply: true,
		},
		{
			name: "live beacon -> conflict",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: path}, OwnerUUID: syncOwner, ExpiresAt: now.Add(time.Hour), Mode: domain.ModeShared, Beacon: true},
			},
			wantReason: syncReasonLeased,
		},
		{
			name: "live territory claim -> conflict",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixParent, OwnerUUID: syncOwner, ExpiresAt: now.Add(time.Hour)},
			},
			wantReason: syncReasonTerritoryClaim,
		},
		{
			// plan-review P1-1: no owner carve-out — even the caller's OWN live
			// territory claim blocks sync. syncDecide takes no myUUID at all.
			name: "own live territory claim still conflicts (P1-1)",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixParent, OwnerUUID: "the-caller-itself", ExpiresAt: now.Add(time.Hour)},
			},
			wantReason: syncReasonTerritoryClaim,
		},
		{
			name: "expired territory claim -> syncable",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixParent, OwnerUUID: syncOwner, ExpiresAt: now.Add(-time.Hour)},
			},
			wantApply: true,
		},
		{
			name: "candidate claim, liveness irrelevant -> conflict",
			cands: []domain.CandidateClaim{
				{PathCanonical: path, CandidateID: "c-1", PID: 999999999},
			},
			wantReason: syncReasonCandidateClaim,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			apply, conflicts := syncDecide(diffs, tc.locks, tc.claims, tc.cands, ec)
			if tc.wantApply {
				if len(apply) != 1 || len(conflicts) != 0 {
					t.Fatalf("want apply, got apply=%d conflicts=%+v", len(apply), conflicts)
				}
				return
			}
			if len(conflicts) != 1 || len(apply) != 0 {
				t.Fatalf("want 1 conflict, got apply=%d conflicts=%+v", len(apply), conflicts)
			}
			if conflicts[0].Reason != tc.wantReason {
				t.Errorf("got reason=%s want %s", conflicts[0].Reason, tc.wantReason)
			}
		})
	}
}

// TestSync_MidApplyFailureReportsWhatItWrote is the loto-8sic regression.
// syncApply is not atomic: it stops at the first write failure, having already
// fast-forwarded everything before it. That list used to be discarded, so the
// operator got exit 3 and a bare error while the tree sat half-changed — the
// opposite of what design.md asks stdout to be.
//
// The failure is forced by making one target read-only on disk, so the blob
// write hits EACCES. Targets are applied in sorted order, so a.go is written
// before z.go fails.
func TestSync_MidApplyFailureReportsWhatItWrote(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not gate root")
	}
	repo := syncBaseRepo(t)
	zPath := filepath.Join(repo, "z.go")
	if err := os.WriteFile(zPath, []byte("zed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "add z")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")

	// Diverge both paths, then take write permission off z.go.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zPath, []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(zPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(zPath, 0o644) })

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("exit %d, want 3; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "partial=true") {
		t.Errorf("a partially applied tree must say so: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ target="+tcTargetA+" action=fast-forward") {
		t.Errorf("the file that WAS written must be named: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "z.go") {
		t.Errorf("the failure must name the path that stopped it: %q", errBuf.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("a.go should have been fast-forwarded before the failure, got %q", got)
	}
}
