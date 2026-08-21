package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

const (
	tcRogueGo = "internal/rogue.go"
	tcSHA1    = "1111111111111111111111111111111111111111"
	tcSHA2    = "2222222222222222222222222222222222222222"
)

func liveEval() domain.EvalContext {
	return domain.EvalContext{Now: time.Now(), Live: liveProbe}
}

func TestMigrate_AddsViolationsTable(t *testing.T) {
	s := mustOpen(t)
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='violations'`).Scan(&n); err != nil {
		t.Fatalf("probe violations table: %v", err)
	}
	if n != 1 {
		t.Fatalf("want violations table present, got count=%d", n)
	}
}

// A path with no live lease is a violation; the row carries the fingerprint
// the sensor saw, not merely the fact that something changed.
func TestReconcileScan_UnleasedPathBecomesViolation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	res, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 1 {
		t.Fatalf("want 1 recorded violation, got %d", len(res.Recorded))
	}
	got := res.Recorded[0]
	if got.PathCanonical != tcRogueGo || got.Fingerprint != tcSHA1 {
		t.Errorf("row = {%s %s}, want {%s %s}", got.PathCanonical, got.Fingerprint, tcRogueGo, tcSHA1)
	}
	if got.LeaseState != LeaseStateUnleased {
		t.Errorf("lease_state = %q, want %q", got.LeaseState, LeaseStateUnleased)
	}
	if got.Resolved() {
		t.Error("a freshly recorded violation must be unresolved")
	}
}

// The stated residual hole, asserted as a property: a leaseholder's own edit
// is NOT a violation, because the sensor reads content and cannot see writers.
func TestReconcileScan_LiveLeaseIsNotAViolation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReconcileScan(ctx,
		[]gate.Observation{{Path: l.Target.Canonical, Fingerprint: tcSHA1}}, liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 0 {
		t.Fatalf("a live leaseholder's own edit must not be a violation, got %d", len(res.Recorded))
	}
}

// A lapsed lease still names the owner it lapsed FROM — forensic annotation,
// never attribution: nothing here claims that owner performed the write.
func TestReconcileScan_ExpiredLeaseRecordsWitnessNotCulprit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Evaluate from a moment past the lease's TTL rather than sleeping.
	ec := domain.EvalContext{Now: time.Now().Add(2 * time.Hour), Live: liveProbe}

	res, err := s.ReconcileScan(ctx,
		[]gate.Observation{{Path: l.Target.Canonical, Fingerprint: tcSHA1}}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 1 {
		t.Fatalf("want 1 recorded violation, got %d", len(res.Recorded))
	}
	if got := res.Recorded[0]; got.LeaseState != LeaseStateExpired || got.ExpectedOwner != tcAlice {
		t.Errorf("witness = {%s %s}, want {%s %s}", got.LeaseState, got.ExpectedOwner, LeaseStateExpired, tcAlice)
	}
}

// A live candidate claim authorizes the path exactly as a lock does: the
// candidate's own commit is precisely the content the tree is carrying.
func TestReconcileScan_LiveCandidateClaimIsNotAViolation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	cc := domain.CandidateClaim{
		PathCanonical: tcRogueGo, CandidateID: tcCand1, OwnerUUID: tcAlice,
		SessionUUID: tcAlice, CreatedAt: time.Now(), Host: tcHost, PID: 1,
	}
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{cc}); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 0 {
		t.Fatalf("a live candidate claim must authorize the path, got %d violations", len(res.Recorded))
	}
}

// THE LAUNDERING TEST. Contaminate an unleased path, then take a perfectly
// valid lease on it. The violation must survive — otherwise `loto lock p &&
// loto submit p` promotes a rogue edit under a lease the gate cannot fault.
func TestReconcileScan_ViolationIsStickyAcrossALaterLease(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	path := l.Target.Canonical

	if _, err := s.ReconcileScan(ctx, []gate.Observation{{Path: path, Fingerprint: tcSHA1}}, liveEval()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Same path, still dirty, now leased — the launderer's happy path.
	if _, err := s.ReconcileScan(ctx, []gate.Observation{{Path: path, Fingerprint: tcSHA2}}, liveEval()); err != nil {
		t.Fatal(err)
	}

	open, err := s.UnresolvedViolationPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := open[path]; !still {
		t.Fatal("violation cleared by a lease taken after it — a rogue edit could be laundered")
	}
}

// Re-observing an already-open path records the FIRST sighting, not the
// latest: how long a path has been dirty is the one thing the row can say.
func TestRecordViolations_ReObservationIsANoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	obs := []ObservedViolation{{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, LeaseState: LeaseStateUnleased}}

	first, err := s.RecordViolations(ctx, obs)
	if err != nil || len(first) != 1 {
		t.Fatalf("first record: %v, n=%d", err, len(first))
	}
	second, err := s.RecordViolations(ctx,
		[]ObservedViolation{{PathCanonical: tcRogueGo, Fingerprint: tcSHA2, LeaseState: LeaseStateUnleased}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("want re-observation to record nothing, got %d rows", len(second))
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("want exactly 1 open row per path, got %d", len(open))
	}
	if open[0].Fingerprint != tcSHA1 || open[0].ObservedAt != first[0].ObservedAt {
		t.Error("re-observation overwrote the first sighting")
	}
}

// A path that leaves the observation set has agreed with integration again —
// the sensor's own evidence that the contamination was reverted.
func TestReconcileScan_RevertAutoResolves(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if _, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, liveEval()); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReconcileScan(ctx, nil, liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 1 {
		t.Fatalf("want 1 auto-resolved violation, got %d", res.Resolved)
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("want no open rows after revert, got %d", len(open))
	}
}

func TestResolveViolation_ClosesOnceAndRefusesTwice(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec, err := s.RecordViolations(ctx,
		[]ObservedViolation{{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, LeaseState: LeaseStateUnleased}})
	if err != nil || len(rec) != 1 {
		t.Fatalf("seed: %v n=%d", err, len(rec))
	}
	id := rec[0].ID

	if err := s.ResolveViolation(ctx, id, ResolutionAcked); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := s.ResolveViolation(ctx, id, ResolutionAcked); !errors.Is(err, ErrUnknownViolation) {
		t.Fatalf("second resolve = %v, want ErrUnknownViolation", err)
	}
	if err := s.ResolveViolation(ctx, "v-nosuch", ResolutionAcked); !errors.Is(err, ErrUnknownViolation) {
		t.Fatalf("unknown id = %v, want ErrUnknownViolation", err)
	}
}

// The resolution string the caller supplies is what lands on the row —
// `loto violations resolve -m "<why>"` promises the reason is recorded, and
// stdout is not a record (Codex #276 P2).
func TestResolveViolation_PersistsTheSuppliedResolution(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec, err := s.RecordViolations(ctx,
		[]ObservedViolation{{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, LeaseState: LeaseStateUnleased}})
	if err != nil || len(rec) != 1 {
		t.Fatalf("seed: %v n=%d", err, len(rec))
	}

	const why = "intentional vendored regen"
	if err := s.ResolveViolation(ctx, rec[0].ID, why); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT resolution FROM violations WHERE id = ?`, rec[0].ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != why {
		t.Errorf("resolution=%q, want %q", got, why)
	}
}

// An ack is a judgment about CONTENT, not just about a row: a later scan
// seeing the same fingerprint on the same still-unleased path must not
// re-open it, or `resolve` would only silence the warning until the next
// scan (Codex #276 P2). A different fingerprint is a new mutation and does
// re-open.
func TestReconcileScan_AckedFingerprintIsNotReopened(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	ec := liveEval()

	res, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, ec)
	if err != nil || len(res.Recorded) != 1 {
		t.Fatalf("seed: %v recorded=%d", err, len(res.Recorded))
	}
	if err := s.ResolveViolation(ctx, res.Recorded[0].ID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}

	again, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Recorded) != 0 {
		t.Errorf("acked fingerprint re-recorded: %+v", again.Recorded)
	}

	changed, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA2}}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.Recorded) != 1 {
		t.Errorf("new content on an acked path was not recorded: %+v", changed.Recorded)
	}
}

// A path may be contaminated, reverted, and contaminated again — each episode
// is its own record. The partial unique index constrains OPEN rows only.
func TestReconcileScan_SecondEpisodeAfterRevertRecordsAgain(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	ec := liveEval()

	if _, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA1}}, ec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileScan(ctx, nil, ec); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Fingerprint: tcSHA2}}, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 1 {
		t.Fatalf("want the second episode recorded, got %d rows", len(res.Recorded))
	}
	if res.Recorded[0].Fingerprint != tcSHA2 {
		t.Errorf("fingerprint = %s, want the second episode's %s", res.Recorded[0].Fingerprint, tcSHA2)
	}
}

// A deleted path is as much an unauthorized mutation as an edit — it has no
// content to hash, and that absence must not read as "nothing happened".
func TestReconcileScan_DeletionIsAViolationWithNoFingerprint(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	res, err := s.ReconcileScan(ctx, []gate.Observation{{Path: tcRogueGo, Deleted: true}}, liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 1 {
		t.Fatalf("want the deletion recorded, got %d rows", len(res.Recorded))
	}
	if res.Recorded[0].Fingerprint != "" {
		t.Errorf("fingerprint = %q, want empty for a deletion", res.Recorded[0].Fingerprint)
	}
}
