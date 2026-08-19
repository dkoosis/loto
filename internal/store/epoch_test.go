package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestMigrate_AddsEpochColumnAndNewTables(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'epoch'`).Scan(&n); err != nil {
		t.Fatalf("probe epoch column: %v", err)
	}
	if n != 1 {
		t.Fatalf("want locks.epoch column present, got count=%d", n)
	}
	for _, table := range []string{"path_epochs", "candidate_claims"} {
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("want table %s present, got count=%d", table, n)
		}
	}
}

// TestAcquireLocks_FreshGrantStartsAtEpochOne: the first-ever lock on a path
// carries epoch 1 — there is no prior generation to distinguish it from.
func TestAcquireLocks_FreshGrantStartsAtEpochOne(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)

	acquired, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if acquired[0].Epoch != 1 {
		t.Errorf("fresh grant epoch = %d, want 1", acquired[0].Epoch)
	}
	got, err := s.LockAt(ctx, l.Target)
	if err != nil || got == nil {
		t.Fatalf("lookup: %v / %v", got, err)
	}
	if got.Epoch != 1 {
		t.Errorf("persisted epoch = %d, want 1", got.Epoch)
	}
}

// TestAcquireLocks_RenewalPreservesEpoch: the same owner re-acquiring its own
// still-live lock (a plain re-run of `loto lock`, or the refresh-shaped upsert
// path) must not bump the epoch — "renewal never self-invalidates healthy
// work" (git-gate.md).
func TestAcquireLocks_RenewalPreservesEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)

	first, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	firstEpoch := first[0].Epoch

	l2 := l
	l2.Intent = "renewed"
	second, err := s.AcquireLocks(ctx, []domain.LockRecord{l2}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Epoch != firstEpoch {
		t.Errorf("renewal epoch = %d, want unchanged %d", second[0].Epoch, firstEpoch)
	}
}

// RefreshLocks never touches the epoch column at all (it UPDATEs only
// expires_at) — this pins that by inspection rather than by re-deriving the
// SQL, since a regression there would be silent (no test currently reads
// epoch after a refresh).
func TestRefreshLocks_PreservesEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LockAt(ctx, l.Target)

	if _, err := s.RefreshLocks(ctx, []domain.Target{l.Target}, domain.AgentUUID(tcAlice), 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	after, _ := s.LockAt(ctx, l.Target)
	if after.Epoch != before.Epoch {
		t.Errorf("refresh changed epoch: %d -> %d", before.Epoch, after.Epoch)
	}
}

// TestAcquireLocks_ReleaseReacquireIncrementsEpoch: release then re-acquire —
// even by the SAME owner — is a fresh grant, not a renewal. The row is gone in
// between; nothing "renews."
func TestAcquireLocks_ReleaseReacquireIncrementsEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)

	first, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Epoch <= first[0].Epoch {
		t.Errorf("release+reacquire epoch = %d, want > %d", second[0].Epoch, first[0].Epoch)
	}
}

// TestAcquireLocks_SelfReacquireAfterOwnExpiryIncrementsEpoch is the edge case
// resolveEpoch has to get right: the SAME owner re-acquiring after its OWN
// lease's TTL lapsed. collectAllBlockers never touches a same-owner row
// (stale or not), so the SQL upsert still hits the UPDATE branch — but the
// epoch decision must not follow the SQL branch here. A lapsed TTL is
// "territory became reclaimable" regardless of who reclaims it, and the plan's
// own words are "stale-owner reclaim... increment[s]" — a self-reclaim after
// expiry is still a reclaim.
func TestAcquireLocks_SelfReacquireAfterOwnExpiryIncrementsEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, -time.Minute) // already expired

	first, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	l2 := l
	l2.ExpiresAt = time.Now().Add(time.Hour)
	second, err := s.AcquireLocks(ctx, []domain.LockRecord{l2}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Epoch <= first[0].Epoch {
		t.Errorf("self-reacquire-after-expiry epoch = %d, want > %d", second[0].Epoch, first[0].Epoch)
	}
}

// TestAcquireLocks_StaleOwnerReclaimIncrementsEpoch_ContinuingCount: a
// DIFFERENT owner reclaiming a dead holder's stale lock must bump the epoch,
// and path_epochs' durability means the new epoch continues counting from the
// dead holder's last value rather than resetting to 1 — the counter tracks the
// PATH's generation, not any one owner's.
func TestAcquireLocks_StaleOwnerReclaimIncrementsEpoch_ContinuingCount(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, -time.Minute) // stale

	first, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, deadProbe)
	if err != nil {
		t.Fatal(err)
	}
	bob := l
	bob.Target = l.Target
	bob.OwnerUUID = tcBob
	bob.SessionUUID = tcBob
	bob.ExpiresAt = time.Now().Add(time.Hour)
	second, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, deadProbe)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Epoch != first[0].Epoch+1 {
		t.Errorf("stale-owner reclaim epoch = %d, want exactly %d (continuing the count)", second[0].Epoch, first[0].Epoch+1)
	}
}

// TestBreakLocks_ThenReacquireIncrementsEpoch: force-break is the fourth case
// git-gate.md enumerates. BreakLocks deletes the row; the next AcquireLocks
// sees no row and takes the fresh-grant path.
func TestBreakLocks_ThenReacquireIncrementsEpoch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)

	first, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakForce, "taking over", liveProbe); err != nil {
		t.Fatal(err)
	}
	bob := l
	bob.OwnerUUID = tcBob
	bob.SessionUUID = tcBob
	second, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Epoch != first[0].Epoch+1 {
		t.Errorf("force-break-then-reacquire epoch = %d, want %d", second[0].Epoch, first[0].Epoch+1)
	}
}

// TestAcquireLocks_EpochsAreIndependentPerPath: two unrelated paths must not
// share a counter — one path's churn must not perturb another's epoch.
func TestAcquireLocks_EpochsAreIndependentPerPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	b := mkFileLock(t, tcBGo, tcAlice, time.Hour)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, tcAlice, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil { // a.go now at epoch 2
		t.Fatal(err)
	}
	acquired, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if acquired[0].Epoch != 1 {
		t.Errorf("b.go's first grant epoch = %d, want 1 (unaffected by a.go's churn)", acquired[0].Epoch)
	}
}

// --- domain.CandidateClaimIsDead --------------------------------------------

func TestCandidateClaimIsDead(t *testing.T) {
	now := time.Now()
	cc := domain.CandidateClaim{
		OwnerUUID: tcAlice, Host: tcHost, PID: 1, ProcStart: 0, CreatedAt: now,
	}
	cases := []struct {
		name string
		ec   domain.EvalContext
		want bool
	}{
		{"nil probe never reports dead", domain.EvalContext{Now: now}, false},
		{"live probe reports not dead", domain.EvalContext{Now: now, Live: liveProbe}, false},
		{"dead probe reports dead", domain.EvalContext{Now: now, Live: deadProbe}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ec.CandidateClaimIsDead(cc); got != c.want {
				t.Errorf("CandidateClaimIsDead() = %v, want %v", got, c.want)
			}
		})
	}
}

// A candidate claim has NO TTL — ExpiresAt does not exist on the type at all,
// so there is nothing to pin far-future in the LockRecord shim; this test
// exists to make that omission a deliberate, checked fact rather than a
// silent one, by asserting a claim with a long CreatedAt in the past is still
// only judged by the probe.
func TestCandidateClaimIsDead_NoTTLBackstop(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	cc := domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost, PID: 1, CreatedAt: old}
	ec := domain.EvalContext{Now: time.Now(), Live: liveProbe}
	if ec.CandidateClaimIsDead(cc) {
		t.Error("a month-old claim with a live process must not be dead — no TTL authority for this record kind")
	}
}

// --- candidate claim store CRUD ---------------------------------------------

func mkCandidateClaim(path, candidateID, owner string) domain.CandidateClaim {
	return domain.CandidateClaim{
		PathCanonical: path, CandidateID: candidateID,
		OwnerUUID: domain.AgentUUID(owner), SessionUUID: domain.SessionUUID(owner),
		CreatedAt: time.Now(), Host: tcHost, PID: 1,
	}
}

func TestInsertCandidateClaimsUnguarded_ThenListRoundTrips(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	claims := []domain.CandidateClaim{
		mkCandidateClaim(tcAGo, tcCand1, tcAlice),
		mkCandidateClaim(tcBGo, tcCand1, tcAlice),
	}
	if err := s.insertCandidateClaimsUnguarded(ctx, claims); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 claims, got %+v", got)
	}
	if got[0].PathCanonical != tcAGo || got[1].PathCanonical != tcBGo {
		t.Errorf("want path-sorted order, got %+v", got)
	}
}

func TestInsertCandidateClaimsUnguarded_EmptyIsNoop(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.insertCandidateClaimsUnguarded(ctx, nil); err != nil {
		t.Fatalf("empty insert must not error: %v", err)
	}
	got, err := s.ListCandidateClaims(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("want no claims, got %+v (err %v)", got, err)
	}
}

func TestReleaseCandidateClaims_DeletesOnlyThatCandidate(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(tcAGo, tcCand1, tcAlice),
		mkCandidateClaim(tcBGo, "cand-2", tcAlice),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCandidateClaims(ctx, tcCand1); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CandidateID != "cand-2" {
		t.Fatalf("want only cand-2 left, got %+v", got)
	}
}

func TestReleaseCandidateClaims_UnknownIDIsNoop(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ReleaseCandidateClaims(ctx, "no-such-candidate"); err != nil {
		t.Errorf("releasing an unknown candidate must not error: %v", err)
	}
}

func TestCandidateClaimsForPaths_FiltersByPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(tcAGo, tcCand1, tcAlice),
		mkCandidateClaim(tcBGo, tcCand1, tcAlice),
		mkCandidateClaim("c.go", "cand-2", tcAlice),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.CandidateClaimsForPaths(ctx, []string{tcAGo, "c.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 matching claims, got %+v", got)
	}
}

func TestCandidateClaimsForPaths_EmptyInputIsNoop(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	got, err := s.CandidateClaimsForPaths(ctx, nil)
	if err != nil || got != nil {
		t.Fatalf("empty paths must return nil, got %+v / %v", got, err)
	}
}

// --- acquisition-time overlap block (loto-ovno.2 part 3) --------------------

func TestAcquireLocks_BlockedByOverlappingCandidateClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(l.Target.Canonical, tcCand1, tcBob),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	var cce *CandidateClaimConflictError
	if !errors.As(err, &cce) {
		t.Fatalf("want CandidateClaimConflictError, got %v", err)
	}
	if len(cce.Blockers) != 1 || cce.Blockers[0].CandidateID != tcCand1 {
		t.Errorf("blockers = %+v, want cand-1", cce.Blockers)
	}
}

// The block applies regardless of whose candidate it is — even the SAME agent
// about to acquire. A candidate's captured preimage goes stale the moment
// ANYONE edits the path again, and the plan states "any unresolved candidate
// claim" with no owner carve-out (unlike ordinary lock conflicts, which do
// exempt the same owner).
func TestAcquireLocks_BlockedEvenBySameOwnersOwnCandidateClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(l.Target.Canonical, tcCand1, tcAlice),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe)
	var cce *CandidateClaimConflictError
	if !errors.As(err, &cce) {
		t.Fatalf("want CandidateClaimConflictError even for the claim's own owner, got %v", err)
	}
}

func TestAcquireLocks_NotBlockedByCandidateClaimOnDifferentPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	elsewhere := mkFileLock(t, "elsewhere.go", tcBob, time.Hour)
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(elsewhere.Target.Canonical, tcCand1, tcBob),
	}); err != nil {
		t.Fatal(err)
	}

	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatalf("an unrelated path must not be blocked: %v", err)
	}
}

// Releasing the claim clears the way — the block reads live store state each
// call, not a cached decision.
func TestAcquireLocks_UnblockedAfterCandidateClaimReleased(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(l.Target.Canonical, tcCand1, tcBob),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCandidateClaims(ctx, tcCand1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatalf("acquire after release must succeed: %v", err)
	}
}

// Multi-target batches must be checked against candidate claims BEFORE any
// row is written — a batch with one clean target and one blocked target must
// leave the clean target untouched, matching the all-or-nothing shape
// TestAcquireLocks_MultiFile_AtomicSuccess already pins for lock conflicts.
func TestAcquireLocks_CandidateClaimBlockAbortsWholeBatch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	clean := mkFileLock(t, "clean.go", tcAlice, time.Hour)
	blocked := mkFileLock(t, "blocked.go", tcAlice, time.Hour)
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{
		mkCandidateClaim(blocked.Target.Canonical, tcCand1, tcBob),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.AcquireLocks(ctx, []domain.LockRecord{clean, blocked}, liveProbe)
	var cce *CandidateClaimConflictError
	if !errors.As(err, &cce) {
		t.Fatalf("want CandidateClaimConflictError, got %v", err)
	}
	if got, _ := s.LockAt(ctx, clean.Target); got != nil {
		t.Errorf("the clean target must not have been locked when the batch aborted, got %+v", got)
	}
}
