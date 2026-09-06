package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

const (
	tcCmdSync = "sync"
	// Residue fixture (loto-ovno.13): a created path a rejection attributes,
	// the candidate id it is charged to, and a stray nothing vouches for.
	tcResidueNewGo     = "new.go"
	tcResidueCandidate = "c-resid0001"
	tcResidueOtherGo   = "other.go"
	tcResidueEnv       = ".env"
	tcResidueBody      = "package p\n"
)

var errInjectedSyncIO = errors.New("injected sync I/O failure")

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

// TestSync_LeavesTargetOverwrittenBetweenClassifyAndApply closes the
// fast-forward half's window — the one syncDeleteResidue has always closed for
// deletions (loto-gai7). The scan hashed the divergent path, sync decided to
// overwrite it, and a peer holding no lease wrote its own work there before the
// apply loop got to it. The pre-write re-probe must catch that: the peer's
// bytes stay, and the row names what it found against what it would have
// written.
func TestSync_LeavesTargetOverwrittenBetweenClassifyAndApply(t *testing.T) {
	repo := syncBaseRepo(t)
	target := filepath.Join(repo, tcTargetA)
	if err := os.WriteFile(target, []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const peerWork = "package p\n\nfunc LandedMidSync() {}\n"
	syncBeforeApplyFn = func() {
		if err := os.WriteFile(target, []byte(peerWork), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { syncBeforeApplyFn = nil })

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if got := readFileT(t, target); got != peerWork {
		t.Errorf("sync overwrote work that landed after classification: %q", got)
	}
	if !strings.Contains(out.String(), "✓ sync synced=0 conflicts=0 skipped=1") {
		t.Errorf("missing triage line: %q", out.String())
	}
	row := "⚠ target=" + tcTargetA + " reason=target-modified" +
		" found=" + shortOID(syncGitT(t, repo, "hash-object", tcTargetA)) +
		" want=" + shortOID(syncGitT(t, repo, "rev-parse", "refs/loto/integration:"+tcTargetA))
	if !strings.Contains(out.String(), row) {
		t.Errorf("missing row %q in: %q", row, out.String())
	}
}

// --- Step 3: conflict red ---

// TestSync_LeaseAcquireCannotRaceFinalApply pauses sync after its final state
// read and proves a peer cannot acquire the target until publication finishes.
func TestSync_LeaseAcquireCannotRaceFinalApply(t *testing.T) {
	repo := syncBaseRepo(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt, err := openRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	readDone := make(chan struct{})
	resume := make(chan struct{})
	syncBeforeApplyFn = func() {
		close(readDone)
		<-resume
	}
	t.Cleanup(func() { syncBeforeApplyFn = nil })

	diff := syncDiff{
		syncEntry: syncEntry{Path: tcTargetA, Mode: "100644", OID: syncGitT(t, repo, "rev-parse", "refs/loto/integration:"+tcTargetA)},
		State:     syncModified,
		// What a real divergence scan would have hashed — the apply half
		// re-probes against it before writing.
		Observed: syncGitT(t, repo, "hash-object", tcTargetA),
	}
	done := make(chan int, 1)
	expectedIntegration := syncGitT(t, repo, "rev-parse", "refs/loto/integration")
	go func() {
		_, code := syncStoreDecideApply(rt, repo, expectedIntegration, []syncDiff{diff}, nil, syncOpts{}, io.Discard)
		done <- code
	}()
	<-readDone

	acquireCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	now := time.Now()
	_, acquireErr := rt.Store.AcquireLocks(acquireCtx, []domain.LockRecord{{
		Target:      domain.Target{Canonical: tcTargetA},
		OwnerUUID:   "racing-owner",
		SessionUUID: "racing-session",
		Intent:      "edit while sync is deciding",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        rt.Host,
		PID:         os.Getpid(),
	}}, nil)
	if !errors.Is(acquireErr, context.DeadlineExceeded) {
		close(resume)
		<-done
		t.Fatalf("lease acquire during sync final apply = %v, want context deadline while op-flock is held", acquireErr)
	}

	close(resume)
	if code := <-done; code != 0 {
		t.Fatalf("sync exit %d, want 0", code)
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("sync did not restore integration content: %q", got)
	}
}

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
	if len(lines) < 4 {
		t.Fatalf("want at least 4 lines, got %d: %q", len(lines), out.String())
	}
	if lines[0] != "✗ sync synced=1 conflicts=1 skipped=0 deleted=0 residue-modified=0 unattributed=0" {
		t.Errorf("bad triage line: %q", lines[0])
	}
	// Line 2 is the attribution-window row: it always follows the triage
	// counts, so a reader who sees deleted=0 also sees how far back sync could
	// have looked before concluding it.
	if !strings.HasPrefix(lines[1], "ℹ attribution=rejected-candidate-write-set+blob window=") {
		t.Errorf("want the attribution-window row second, got: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "✓ target="+tcTargetA+" action=fast-forward") {
		t.Errorf("want a.go row before b.go (path-sorted), got: %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "✗ target="+tcTargetB+" reason=leased holder=") {
		t.Errorf("want b.go conflict row third, got: %q", lines[3])
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

// --- v2: untracked residue, deleted only by recorded attribution (loto-ovno.13) ---

// blobOf is the SHA git would store the given bytes under — the value a real
// envelope's Transition.Result carries for a created path.
func blobOf(t *testing.T, repo, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Dir = repo
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// recordRejection persists a candidate_rejected verdict naming the paths that
// candidate created, each bound to the blob it wrote there — the only
// attribution `loto sync` will delete by. Uses its own short-lived runtime and
// closes it before the caller runs sync, so the sweep sees the row through a
// fresh open, not a shared handle.
func recordRejection(t *testing.T, candidateID string, created ...gate.CreatedPath) {
	t.Helper()
	rt, err := openRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Store.RecordAdmissionVerdict(rt.Ctx, rt.Agent.UUID, candidateID,
		gate.ReasonStalePreimage, created); err != nil {
		t.Fatal(err)
	}
}

// plantResidue writes an untracked file and records the rejection that
// attributes it, blob and all — the fixture every deletion case starts from.
func plantResidue(t *testing.T, repo, path, content string) string {
	t.Helper()
	full := filepath.Join(repo, path)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recordRejection(t, tcResidueCandidate, gate.CreatedPath{Path: path, Blob: blobOf(t, repo, content)})
	return full
}

// TestSync_DeletesResidueAttributedToARejectedCandidate is the whole bead: an
// untracked file a rejected candidate is on record as having created is
// removed, and the row names both the path and the candidate the deletion is
// charged to (DESIGN.md invariant 8 — no silent dispossession).
func TestSync_DeletesResidueAttributedToARejectedCandidate(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=0 conflicts=0 skipped=0 deleted=1 residue-modified=0 unattributed=0") {
		t.Errorf("missing triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "ℹ target="+tcResidueNewGo+" action=delete candidate="+tcResidueCandidate) {
		t.Errorf("deletion row must name path and candidate: %q", out.String())
	}
	if _, err := os.Stat(residue); !os.IsNotExist(err) {
		t.Errorf("attributed residue survived: %v", err)
	}
	if !strings.Contains(out.String(), "ℹ attribution=rejected-candidate-write-set+blob window=") {
		t.Errorf("report must state the attribution window: %q", out.String())
	}
}

// TestSync_LeavesResidueRecreatedWithDifferentBytes is the review's correction:
// a peer re-created the same path with their own work. The path still matches
// the rejection's record, the CONTENT does not — and a deletion restores
// nothing, so it must be exact.
func TestSync_LeavesResidueRecreatedWithDifferentBytes(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)
	const peerWork = "package p\n\nfunc Peer() {}\n"
	if err := os.WriteFile(residue, []byte(peerWork), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync, "--verbose"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=0 conflicts=0 skipped=0 deleted=0 residue-modified=1 unattributed=0") {
		t.Errorf("missing triage line: %q", out.String())
	}
	recorded := shortOID(blobOf(t, repo, tcResidueBody))
	found := shortOID(blobOf(t, repo, peerWork))
	wantRow := "⚠ target=" + tcResidueNewGo + " reason=" + syncReasonResidueModified +
		" candidate=" + tcResidueCandidate + " recorded=" + recorded + " found=" + found
	if !strings.Contains(out.String(), wantRow) {
		t.Errorf("--verbose row must name both SHAs, want %q in: %q", wantRow, out.String())
	}
	if got := readFileT(t, residue); got != peerWork {
		t.Errorf("sync deleted a peer's work at an attributed path: %q", got)
	}
}

// TestSync_RefusesResidueThatIsNoLongerARegularFile: between the scan and the
// delete a residue path can turn into a directory or a symlink. Neither is the
// blob this run was authorized to remove, so both are reported and skipped —
// and os.Remove would have happily unlinked the symlink.
func TestSync_RefusesResidueThatIsNoLongerARegularFile(t *testing.T) {
	repo := syncBaseRepo(t)
	blob := blobOf(t, repo, tcResidueBody)

	dirPath := filepath.Join(repo, tcResidueNewGo)
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(repo, tcResidueOtherGo)
	if err := os.Symlink(filepath.Join(repo, tcTargetA), linkPath); err != nil {
		t.Fatal(err)
	}

	deleted, skips, modified, err := syncDeleteResidue(t.Context(), repo, []syncResidue{
		{Path: tcResidueNewGo, CandidateID: tcResidueCandidate, Blob: blob},
		{Path: tcResidueOtherGo, CandidateID: tcResidueCandidate, Blob: blob},
	})
	if err != nil {
		t.Fatalf("delete returned an error rather than skipping: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v, want nothing", deleted)
	}
	if len(modified) != 0 {
		t.Errorf("a non-file was reported as modified rather than not-regular: %v", modified)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %v, want one per path", skips)
	}
	for _, s := range skips {
		if s.Reason != syncReasonResidueNotFile {
			t.Errorf("%s skipped with reason %q, want %q", s.Path, s.Reason, syncReasonResidueNotFile)
		}
	}
	if fi, err := os.Lstat(dirPath); err != nil || !fi.IsDir() {
		t.Errorf("the directory was removed: %v", err)
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Errorf("the symlink was removed: %v", err)
	}
}

// TestSync_LeavesResidueOverwrittenBetweenClassifyAndDelete closes the window
// the classification pass opens: sync decided this file was deletable, then a
// peer wrote its own work into it before the delete loop reached it. The
// re-probe must catch that — re-checking only the file TYPE would still have
// unlinked the peer's bytes.
func TestSync_LeavesResidueOverwrittenBetweenClassifyAndDelete(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)

	const peerWork = "package p\n\nfunc LandedMidSync() {}\n"
	syncBeforeDeleteFn = func() {
		if err := os.WriteFile(residue, []byte(peerWork), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { syncBeforeDeleteFn = nil })

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=0 conflicts=0 skipped=0 deleted=0 residue-modified=1 unattributed=0") {
		t.Errorf("missing triage line: %q", out.String())
	}
	if got := readFileT(t, residue); got != peerWork {
		t.Errorf("sync deleted work that landed after classification: %q", got)
	}
}

// TestSync_ResidueVanishingMidRunDoesNotAbortTheSync: a peer cleaned up its own
// residue between the untracked scan and the content probe. That path is simply
// gone — it must cost nothing but itself, and the rest of the repair must
// finish. (Before per-file probing, a batched `git hash-object --stdin-paths`
// exited 128 on the missing path and took the whole sync down with it.)
func TestSync_ResidueVanishingMidRunDoesNotAbortTheSync(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fires after the coordination read, before classification — the scan has
	// already listed the file and attributed it.
	syncBeforeApplyFn = func() {
		if err := os.Remove(residue); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { syncBeforeApplyFn = nil })

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=1 conflicts=0 skipped=0 deleted=0 residue-modified=0 unattributed=0") {
		t.Errorf("a vanished residue path must cost nothing but itself: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ target="+tcTargetA+" action=fast-forward") {
		t.Errorf("the rest of the repair must still run: %q", out.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "" {
		t.Errorf("divergent path not restored: %q", got)
	}
}

// TestSync_LeavesUnattributedUntrackedFile: nothing vouches for a stray .env,
// so it is counted and left alone. This is the fail-closed half — sync must
// never reason from "untracked" to "disposable".
func TestSync_LeavesUnattributedUntrackedFile(t *testing.T) {
	repo := syncBaseRepo(t)
	stray := filepath.Join(repo, tcResidueEnv)
	if err := os.WriteFile(stray, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync, "--verbose"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ sync synced=0 conflicts=0 skipped=0 deleted=0 residue-modified=0 unattributed=1") {
		t.Errorf("missing triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "ℹ target="+tcResidueEnv+" reason=unattributed action=none") {
		t.Errorf("--verbose must name the unattributed path: %q", out.String())
	}
	if got := readFileT(t, stray); got != "SECRET=1\n" {
		t.Errorf("unattributed file was touched: %q", got)
	}
}

// TestSync_LeavesPathCommittedSinceTheRejection: someone committed the file a
// rejected candidate created, so it is tracked content now — not residue. The
// untracked listing is what enforces this, and the test pins it: attribution
// alone must never be enough to delete.
func TestSync_LeavesPathCommittedSinceTheRejection(t *testing.T) {
	repo := syncBaseRepo(t)
	kept := filepath.Join(repo, tcResidueNewGo)
	if err := os.WriteFile(kept, []byte(tcResidueBody), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", tcResidueNewGo)
	syncGitT(t, repo, "commit", "-q", "-m", "someone committed the residue")
	// Integration moves with HEAD; otherwise sync refuses as behind-HEAD and
	// the test would pass without ever reaching the residue pass.
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	recordRejection(t, tcResidueCandidate,
		gate.CreatedPath{Path: tcResidueNewGo, Blob: blobOf(t, repo, tcResidueBody)})

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "action=delete") {
		t.Errorf("sync deleted a tracked path: %q", out.String())
	}
	if got := readFileT(t, kept); got != tcResidueBody {
		t.Errorf("tracked file was touched: %q", got)
	}
}

// TestSync_DryRunDeletesNothing: --dry-run reports the same decision and
// leaves the tree exactly as it found it, in both halves — the fast-forward
// and the deletion.
func TestSync_DryRunDeletesNothing(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync, "--dry-run"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "ℹ sync dry-run=true would-fast-forward=1 conflicts=0 skipped=0 would-delete=1 residue-modified=0 unattributed=0") {
		t.Errorf("missing dry-run triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "ℹ target="+tcResidueNewGo+" action=would-delete candidate="+tcResidueCandidate) {
		t.Errorf("missing would-delete row: %q", out.String())
	}
	if got := readFileT(t, residue); got != tcResidueBody {
		t.Errorf("--dry-run deleted the residue: %q", got)
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "drift\n" {
		t.Errorf("--dry-run fast-forwarded a divergent path: %q", got)
	}
}

// TestSync_RefusesToDeleteLeasedResidue: a peer has taken a lease on the file
// the rejected candidate left behind — it is someone's live work now. The
// deletion answers to every holder v1 already refuses to write over.
func TestSync_RefusesToDeleteLeasedResidue(t *testing.T) {
	repo := syncBaseRepo(t)
	residue := plantResidue(t, repo, tcResidueNewGo, tcResidueBody)
	if code := Run([]string{tcCmdLock, tcResidueNewGo, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit %d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✗ sync synced=0 conflicts=1 skipped=0 deleted=0 residue-modified=0 unattributed=0") {
		t.Errorf("missing triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "✗ target="+tcResidueNewGo+" reason="+syncReasonLeased) {
		t.Errorf("missing leased conflict row: %q", out.String())
	}
	if got := readFileT(t, residue); got != tcResidueBody {
		t.Errorf("sync deleted a leased file: %q", got)
	}
}

// TestSync_RejectsStrayArgs: sync takes no positional arguments.
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

func TestSync_RefusesStaleIntegrationSnapshot(t *testing.T) {
	repo := syncBaseRepo(t)
	expectedIntegration := syncGitT(t, repo, "rev-parse", "refs/loto/integration")
	oldOID := syncGitT(t, repo, "rev-parse", expectedIntegration+":"+tcTargetA)

	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("promoted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "--", tcTargetA)
	syncGitT(t, repo, "commit", "-q", "-m", "advance integration")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt, err := openRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var errBuf bytes.Buffer
	out, code := syncStoreDecideApply(rt, repo, expectedIntegration, []syncDiff{{
		syncEntry: syncEntry{Path: tcTargetA, Mode: "100644", OID: oldOID},
		State:     syncModified,
	}}, nil, syncOpts{}, &errBuf)
	if code != 3 || !strings.Contains(errBuf.String(), errSyncIntegrationChanged.Error()) {
		t.Fatalf("stale sync = code %d synced=%v err=%q, want refusal", code, out.Synced, errBuf.String())
	}
	if got := readFileT(t, filepath.Join(repo, tcTargetA)); got != "worktree\n" {
		t.Errorf("stale sync overwrote current worktree bytes: %q", got)
	}
}

func TestSync_WriteFailurePreservesExistingFile(t *testing.T) {
	repo := syncBaseRepo(t)
	path := filepath.Join(repo, tcTargetA)
	const old = "work in progress\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	oid := syncGitT(t, repo, "rev-parse", "refs/loto/integration:"+tcTargetA)

	writeErr := errInjectedSyncIO
	originalWrite := syncWriteFn
	syncWriteFn = func(f *os.File, _ []byte) (int, error) {
		n, err := f.WriteString("x")
		if err != nil {
			return n, err
		}
		return n, writeErr
	}
	t.Cleanup(func() { syncWriteFn = originalWrite })

	published, err := syncApplyOne(t.Context(), repo, syncDiff{
		syncEntry: syncEntry{Path: tcTargetA, Mode: "100644", OID: oid},
		State:     syncModified,
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("syncApplyOne error = %v, want %v", err, writeErr)
	}
	if published {
		t.Error("failed pre-publication write reported the target as published")
	}
	if got := readFileT(t, path); got != old {
		t.Errorf("failed replacement changed target: got %q want %q", got, old)
	}
	matches, err := filepath.Glob(filepath.Join(repo, ".loto-sync-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temporary files remain after failure: %v", matches)
	}
}

func TestSync_NewlinePath(t *testing.T) {
	repo := syncBaseRepo(t)
	const path = "line\nbreak.txt"
	full := filepath.Join(repo, path)
	if err := os.WriteFile(full, []byte("integrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "--", path)
	syncGitT(t, repo, "commit", "-q", "-m", "add newline path")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	if err := os.WriteFile(full, []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSync}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, want 0; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if got := readFileT(t, full); got != "integrated\n" {
		t.Errorf("newline path content = %q, want integration content", got)
	}
	wantRow := "✓ target=\"line\\nbreak.txt\" action=fast-forward\n"
	if !strings.Contains(out.String(), wantRow) {
		t.Errorf("newline path missing from success report: %q", out.String())
	}
	if strings.Contains(out.String(), "target="+path) {
		t.Errorf("newline path injected a second output row: %q", out.String())
	}
}

func TestSync_PostPublishFailureReportsCurrentPath(t *testing.T) {
	repo := syncBaseRepo(t)
	path := filepath.Join(repo, tcTargetA)
	if err := os.WriteFile(path, []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oid := syncGitT(t, repo, "rev-parse", "refs/loto/integration:"+tcTargetA)

	syncErr := errInjectedSyncIO
	originalSyncParentDir := syncParentDirFn
	syncParentDirFn = func(string) error { return syncErr }
	t.Cleanup(func() { syncParentDirFn = originalSyncParentDir })

	synced, _, err := syncApply(t.Context(), repo, []syncDiff{{
		syncEntry: syncEntry{Path: tcTargetA, Mode: "100644", OID: oid},
		State:     syncModified,
		Observed:  syncGitT(t, repo, "hash-object", tcTargetA),
	}})
	if !errors.Is(err, syncErr) {
		t.Fatalf("syncApply error = %v, want %v", err, syncErr)
	}
	if len(synced) != 1 || synced[0] != tcTargetA {
		t.Errorf("published paths = %v, want [%s]", synced, tcTargetA)
	}
	if got := readFileT(t, path); got != "" {
		t.Errorf("published content = %q, want integration content", got)
	}
}

// TestSync_MidApplyFailureReportsWhatItWrote is the loto-8sic regression.
// syncApply is not atomic: it stops at the first write failure, having already
// fast-forwarded everything before it. That list used to be discarded, so the
// operator got exit 3 and a bare error while the tree sat half-changed — the
// opposite of what design.md asks stdout to be.
//
// The failure is injected into the second temporary-file write. Targets are
// applied in sorted order, so a.go is published before z.go fails, while z.go's
// pre-sync bytes remain intact.
func TestSync_MidApplyFailureReportsWhatItWrote(t *testing.T) {
	repo := syncBaseRepo(t)
	zPath := filepath.Join(repo, "z.go")
	if err := os.WriteFile(zPath, []byte("zed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	syncGitT(t, repo, "add", "-A")
	syncGitT(t, repo, "commit", "-q", "-m", "add z")
	syncGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")

	// Diverge both paths, then fail the second atomic-replacement write.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zPath, []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeErr := errInjectedSyncIO
	originalWrite := syncWriteFn
	calls := 0
	syncWriteFn = func(f *os.File, data []byte) (int, error) {
		calls++
		if calls == 2 {
			return 0, writeErr
		}
		return f.Write(data)
	}
	t.Cleanup(func() { syncWriteFn = originalWrite })

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
	if got := readFileT(t, zPath); got != "drift\n" {
		t.Errorf("z.go changed despite pre-publication failure: %q", got)
	}
}

// TestSyncMkdirAll_FsyncsEveryCreatedLevel pins the loto-8sic PR review fix
// (Codex): repairing a path whose parent directories were also deleted must
// fsync each newly created level's parent, not just the deepest directory —
// otherwise a crash can drop the repaired hierarchy even though the file
// write itself reported success. φ atomicfile's
// TestMkdirAllSyncFsyncsEveryCreatedParent.
func TestSyncMkdirAll_FsyncsEveryCreatedLevel(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	var synced []string
	original := syncParentDirFn
	syncParentDirFn = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { syncParentDirFn = original })

	if err := syncMkdirAll(target, 0o755); err != nil {
		t.Fatalf("syncMkdirAll: %v", err)
	}

	// Created: root/a, root/a/b, root/a/b/c — so their parents, in that order.
	want := []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")}
	if !slices.Equal(synced, want) {
		t.Errorf("fsynced dirs = %v, want %v", synced, want)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Errorf("target dir not created: stat=%v err=%v", fi, err)
	}
}

// TestSyncMkdirAll_SkipsExistingDir asserts the no-op path: an
// already-existing directory gets no fsync calls at all.
func TestSyncMkdirAll_SkipsExistingDir(t *testing.T) {
	root := t.TempDir()

	var synced []string
	original := syncParentDirFn
	syncParentDirFn = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { syncParentDirFn = original })

	if err := syncMkdirAll(root, 0o755); err != nil {
		t.Fatalf("syncMkdirAll: %v", err)
	}
	if len(synced) != 0 {
		t.Errorf("fsynced %v for an already-existing dir, want none", synced)
	}
}
