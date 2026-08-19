package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

// gitT/writeTestFile mirror internal/gate's own test harness
// (envelope_test.go) — duplicated rather than exported, since exporting
// test-only helpers across packages for two call sites isn't worth the API
// surface.
const tcBead = "loto-ovno.4"

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=base", "GIT_AUTHOR_EMAIL=base@t",
		"GIT_COMMITTER_NAME=base", "GIT_COMMITTER_EMAIL=base@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newGateRepo builds a throwaway git repo (independent of this file's own
// loto DB) for the git-plumbing half of AcceptCandidate — envelope capture and
// ref writes operate on a real repo, not the sqlite store.
func newGateRepo(t *testing.T) (repoTop, integration string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")

	repoTop = t.TempDir()
	gitT(t, repoTop, "init", "-q", "-b", "main")
	gitT(t, repoTop, "config", "commit.gpgsign", "false")
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 1\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "base")
	return repoTop, gitT(t, repoTop, "rev-parse", "HEAD")
}

// buildProposal stages the working tree and commits it via plain plumbing
// (add + write-tree + commit-tree) — this file doesn't need lane.Commit's
// isolated-index machinery, just a real child commit of integration for
// gate.Capture to read.
func buildProposal(t *testing.T, repoTop, integration string) string {
	t.Helper()
	gitT(t, repoTop, "add", "-A")
	tree := gitT(t, repoTop, "write-tree")
	return gitT(t, repoTop, "commit-tree", tree, "-p", integration, "-m", "wip")
}

// leaseWriteSet takes a real exclusive lease on each repo-relative path and
// returns the epoch map an envelope captured under those leases would carry.
// AcceptCandidate now revalidates the leases as it claims (loto-ovno.10), so a
// test that means to be ACCEPTED has to hold them for real. Canonical targets
// are repo-relative, and file validation stats them against the process CWD —
// hence the chdir, which t.Chdir undoes at test end.
func leaseWriteSet(t *testing.T, s *Store, repoTop string, owner domain.AgentUUID, paths ...string) map[string]int64 {
	t.Helper()
	t.Chdir(repoTop)
	now := time.Now()
	recs := make([]domain.LockRecord, len(paths))
	for i, p := range paths {
		recs[i] = domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   owner,
			SessionUUID: domain.SessionUUID(owner),
			Intent:      tcTest,
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        tcHost,
			PID:         1,
			Mode:        domain.ModeExclusive,
		}
	}
	got, err := s.AcquireLocks(context.Background(), recs, liveProbe)
	if err != nil {
		t.Fatalf("lease write-set: %v", err)
	}
	epochs := make(map[string]int64, len(got))
	for i := range got {
		epochs[got[i].Target.Canonical] = got[i].Epoch
	}
	return epochs
}

func TestAcceptCandidate_WritesRefsAndDurableClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)

	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cand-1", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}

	submitter := domain.CandidateClaim{
		OwnerUUID: tcAlice, SessionUUID: tcAlice, Host: tcHost, PID: 1,
	}
	envSHA, err := s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if envSHA == "" {
		t.Fatal("AcceptCandidate returned an empty envelope SHA")
	}

	if got := gitT(t, repoTop, "rev-parse", "refs/loto/candidates/cand-1"); got != envSHA {
		t.Errorf("candidates ref = %s, want %s", got, envSHA)
	}
	if got := gitT(t, repoTop, "rev-parse", "refs/loto/proposals/cand-1"); got != proposal {
		t.Errorf("proposals ref = %s, want %s", got, proposal)
	}

	claims, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].PathCanonical != tcAGo || claims[0].CandidateID != "cand-1" {
		t.Fatalf("want one durable claim for a.go/cand-1, got %+v", claims)
	}
	if claims[0].OwnerUUID != tcAlice {
		t.Errorf("claim owner = %s, want %s", claims[0].OwnerUUID, tcAlice)
	}
}

// A candidate touching several paths must mint a claim for every one of them
// — the whole point of the durable claim is protecting every path the
// candidate is under review for, not just the first.
func TestAcceptCandidate_MintsOneClaimPerWriteSetPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcBGo, "package x\n\nvar B = 1\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcBGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcBGo}, CandidateID: "cand-2", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}, liveProbe); err != nil {
		t.Fatal(err)
	}
	claims, err := s.CandidateClaimsForPaths(ctx, []string{tcBGo})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("want 1 claim for b.go, got %+v", claims)
	}
}

// Re-accepting the SAME candidate id must fail loudly rather than silently
// overwrite whichever candidate got there first — the double-admission guard.
// Since loto-ovno.10 put the claim insert first, the second attempt trips the
// candidate_claims primary key before it ever reaches WriteCandidateRefs'
// "create"; either way the refs written by the first accept stand unchanged.
func TestAcceptCandidate_RejectsDuplicateCandidateID(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "dup", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}
	submitter := domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe); err == nil {
		t.Fatal("want an error re-accepting the same candidate id, got nil")
	}
}

func TestRecordGateBypass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RecordGateBypass(ctx, tcAlice, "gate binary missing"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = 'gate_bypass' AND actor_uuid = ?`, tcAlice,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 gate_bypass event, got %d", n)
	}
}

func TestMigrate_EventsCheckAllowsGateBypass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	var ddl string
	if err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "'gate_bypass'") {
		t.Errorf("events CHECK constraint must allow 'gate_bypass', got: %s", ddl)
	}
}

// --- claim-time lease revalidation (loto-ovno.10) ---------------------------
//
// The window: runSubmit reads each path's epoch, gate.Admit judges on that
// map, and only then does AcceptCandidate claim. Nothing held the op-flock
// across that gap, so a lease released and regranted inside it left admission
// judging on a stale epoch — and the store used to insert the claim anyway.

func TestAcceptCandidate_RefusesLeaseRegrantedAfterAdmission(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cand-race", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The race, committed: release and re-acquire bumps the epoch (loto-ovno.2).
	target := domain.Target{Canonical: tcAGo}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}
	if got := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo); got[tcAGo] == epochs[tcAGo] {
		t.Fatalf("re-acquire must bump the epoch, still %d", got[tcAGo])
	}

	submitter := domain.CandidateClaim{OwnerUUID: tcAlice, SessionUUID: tcAlice, Host: tcHost, PID: 1}
	_, err = s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe)
	var lre *LeaseRevalidationError
	if !errors.As(err, &lre) {
		t.Fatalf("want *LeaseRevalidationError, got %v", err)
	}
	if len(lre.Conflicts) != 1 || lre.Conflicts[0].Reason != ClaimLeaseEpochChanged {
		t.Errorf("conflicts = %+v, want one lease-epoch-changed", lre.Conflicts)
	}
	assertNoCandidateResidue(t, s, repoTop)
}

// The failure this bead names outright: a live PEER lock and this candidate's
// claim on the same path. Force-breaking Alice's lease and granting Bob's is
// exactly what the unguarded insert could not see.
func TestAcceptCandidate_RefusesWhenPeerHoldsTheLease(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cand-peer", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}

	target := domain.Target{Canonical: tcAGo}
	if _, err := s.BreakLocks(ctx, []domain.Target{target}, tcBob, BreakForce, "peer takeover", liveProbe); err != nil {
		t.Fatal(err)
	}
	leaseWriteSet(t, s, repoTop, tcBob, tcAGo)

	submitter := domain.CandidateClaim{OwnerUUID: tcAlice, SessionUUID: tcAlice, Host: tcHost, PID: 1}
	_, err = s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe)
	var lre *LeaseRevalidationError
	if !errors.As(err, &lre) {
		t.Fatalf("want *LeaseRevalidationError, got %v", err)
	}
	if len(lre.Conflicts) != 1 || lre.Conflicts[0].Reason != ClaimLeaseGone {
		t.Errorf("conflicts = %+v, want one lease-gone", lre.Conflicts)
	}
	// The bead's acceptance criterion, stated as an assertion: never both.
	claims, err := s.CandidateClaimsForPaths(ctx, []string{tcAGo})
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.LockAt(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if held != nil && len(claims) > 0 {
		t.Fatalf("peer lock (%s) AND candidate claim (%+v) on the same path", held.OwnerUUID, claims)
	}
	assertNoCandidateResidue(t, s, repoTop)
}

// A lease that merely lapsed — nobody took it, it just expired — is refused
// too: the authorization the envelope was captured under is gone either way.
func TestAcceptCandidate_RefusesExpiredLease(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cand-expired", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE locks SET expires_at = ? WHERE target_canonical = ?`,
		time.Now().Add(-time.Hour).UnixNano(), tcAGo); err != nil {
		t.Fatal(err)
	}

	submitter := domain.CandidateClaim{OwnerUUID: tcAlice, SessionUUID: tcAlice, Host: tcHost, PID: 1}
	_, err = s.AcceptCandidate(ctx, repoTop, env, submitter, liveProbe)
	var lre *LeaseRevalidationError
	if !errors.As(err, &lre) {
		t.Fatalf("want *LeaseRevalidationError, got %v", err)
	}
	if lre.Conflicts[0].Reason != ClaimLeaseStale {
		t.Errorf("reason = %s, want lease-stale", lre.Conflicts[0].Reason)
	}
	assertNoCandidateResidue(t, s, repoTop)
}

// A refused claim must leave nothing behind — no rows, and no refs for a
// candidate that never landed. This is why AcceptCandidate claims before it
// writes refs (loto-ovno.10).
func assertNoCandidateResidue(t *testing.T, s *Store, repoTop string) {
	t.Helper()
	claims, err := s.ListCandidateClaims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("refused accept left claims behind: %+v", claims)
	}
	if refs := gitT(t, repoTop, "for-each-ref", "--format=%(refname)", "refs/loto/"); refs != "" {
		t.Errorf("refused accept left refs behind: %q", refs)
	}
}

// A caller that goes away mid-acceptance (Ctrl-C) must not strand its claims.
// Codex #261 P1: the compensating release used to run on the caller's own
// context, so the very cancellation that failed WriteBlob also failed the
// cleanup — leaving durable claims with no candidate refs behind them, and no
// reclamation path to clear them. Fails with a ctx-bound compensating delete.
func TestAcceptCandidate_CompensatesClaimsWhenCallerCancels(t *testing.T) {
	s := mustOpen(t)
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cancelled", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the moment the claim insert commits — the exact window where the
	// rows are durable and the git side has not started.
	origCommit := commitTxFn
	defer func() { commitTxFn = origCommit }()
	commitTxFn = func(tx *sql.Tx) error {
		err := origCommit(tx)
		commitTxFn = origCommit
		cancel()
		return err
	}

	if _, err := s.AcceptCandidate(ctx, repoTop, env, domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}, liveProbe); err == nil {
		t.Fatal("want AcceptCandidate to fail on the cancelled context, got nil")
	}

	claims, err := s.ListCandidateClaims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("cancelled acceptance stranded %d claim(s): %+v", len(claims), claims)
	}
}

// Compensation undoes THIS attempt's rows and nothing else. Codex #261 P2: a
// candidate-wide DELETE also clears claims an already-accepted candidate holds
// under the same id, leaving its proposal unprotected. NewCandidateID makes
// that collision unlikely, so the guarantee is asserted at the store's own
// door rather than left resting on id uniqueness.
func TestAcceptCandidate_CompensationSparesOtherPathsOfSameCandidateID(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)
	epochs := leaseWriteSet(t, s, repoTop, tcAlice, tcAGo)

	// A claim already standing under the same candidate id, on a path outside
	// this attempt's write set.
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{{
		PathCanonical: tcBGo, CandidateID: "shared", OwnerUUID: tcAlice,
		CreatedAt: time.Now(), Host: tcHost,
	}}); err != nil {
		t.Fatal(err)
	}
	// Occupy the candidates ref so WriteCandidateRefs' create fails and the
	// compensating release runs.
	gitT(t, repoTop, "update-ref", "refs/loto/candidates/shared", integration)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "shared", BeadID: tcBead,
		LeaseEpoch: epochs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}, liveProbe); err == nil {
		t.Fatal("want AcceptCandidate to fail on the occupied candidates ref, got nil")
	}

	claims, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].PathCanonical != tcBGo {
		t.Fatalf("want the preexisting b.go claim intact and a.go compensated, got %+v", claims)
	}
}
