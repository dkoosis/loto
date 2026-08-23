package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

const (
	tcRogueGo   = "internal/rogue.go"
	tcSHA1      = "1111111111111111111111111111111111111111"
	tcSHA2      = "2222222222222222222222222222222222222222"
	tcBaseline  = "b000000000000000000000000000000000000000"
	tcBaseline2 = "b111111111111111111111111111111111111111"
	tcOtherGo   = "internal/other.go"
)

// scanOf wraps observations as one whole-tree pass against tcBaseline — the
// shape ReconcileScan consumes now that a reading carries what it was a delta
// FROM.
func scanOf(obs ...gate.Observation) gate.Scan {
	return gate.Scan{Baseline: tcBaseline, Observations: obs}
}

// scanFrom is scanOf for a named checkout — the linked-worktree half of the
// sharing the store documents (store.go: "two worktrees of one repo share a
// store").
func scanFrom(worktree string, obs ...gate.Observation) gate.Scan {
	return gate.Scan{Baseline: tcBaseline, Worktree: worktree, Observations: obs}
}

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

	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval())
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
		scanOf(gate.Observation{Path: l.Target.Canonical, Fingerprint: tcSHA1}), liveEval())
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
		scanOf(gate.Observation{Path: l.Target.Canonical, Fingerprint: tcSHA1}), ec)
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

	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval())
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

	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: path, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Same path, still dirty, now leased — the launderer's happy path.
	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: path, Fingerprint: tcSHA2}), liveEval()); err != nil {
		t.Fatal(err)
	}

	open, err := s.UnresolvedViolationPaths(ctx, "")
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
	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReconcileScan(ctx, scanOf(), liveEval())
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

// An ack is scoped to the BASELINE it was given. Once integration moves, the
// same path+fingerprint can mean the opposite thing, so the old ack must stop
// suppressing it (Codex #276 round 2).
//
// The sharp case is a deletion, whose fingerprint is always empty: ack one,
// let integration absorb it, and later have the file reintroduced upstream —
// leaving it absent locally is now a NEW unauthorized deletion, and a
// baseline-blind ack would suppress it forever.
func TestReconcileScan_AckDoesNotSurviveABaselineChange(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	ec := liveEval()

	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Deleted: true}), ec)
	if err != nil || len(res.Recorded) != 1 {
		t.Fatalf("seed: %v recorded=%d", err, len(res.Recorded))
	}
	if err := s.ResolveViolation(ctx, res.Recorded[0].ID, "deletion is intentional"); err != nil {
		t.Fatal(err)
	}
	// Same baseline: the ack holds.
	same, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Deleted: true}), ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Recorded) != 0 {
		t.Errorf("ack did not hold against its own baseline: %+v", same.Recorded)
	}

	// Integration moved. The same absent file is a fresh unauthorized
	// deletion against the new baseline, and must be recorded again.
	moved := gate.Scan{
		Baseline:     tcBaseline2,
		Observations: []gate.Observation{{Path: tcRogueGo, Deleted: true}},
	}
	after, err := s.ReconcileScan(ctx, moved, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Recorded) != 1 {
		t.Errorf("ack survived a baseline change — a later deletion is suppressed forever: %+v", after.Recorded)
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

	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), ec)
	if err != nil || len(res.Recorded) != 1 {
		t.Fatalf("seed: %v recorded=%d", err, len(res.Recorded))
	}
	if err := s.ResolveViolation(ctx, res.Recorded[0].ID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}

	again, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Recorded) != 0 {
		t.Errorf("acked fingerprint re-recorded: %+v", again.Recorded)
	}

	changed, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA2}), ec)
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

	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), ec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileScan(ctx, scanOf(), ec); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA2}), ec)
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

	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Deleted: true}), liveEval())
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

// A clean pass from one checkout must not resolve another checkout's
// violations. The store is shared across worktrees by design, so a row that
// cannot say WHICH tree it came from lets a dispatch worktree launder the
// main checkout's contamination away while the rogue content is still on
// disk (Codex #276 round 2, loto-nper).
func TestReconcileScan_AnotherWorktreesCleanPassLeavesTheRowOpen(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReconcileScan(ctx, scanFrom("agent-b"), liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 0 {
		t.Errorf("a clean pass from agent-b resolved %d row(s) belonging to the primary worktree", res.Resolved)
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].PathCanonical != tcRogueGo {
		t.Fatalf("primary worktree's violation gone: %+v", open)
	}
	// ...and the primary's own clean pass still resolves it, so scoping did
	// not simply disable auto-resolution.
	res, err = s.ReconcileScan(ctx, scanOf(), liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 1 {
		t.Errorf("primary's own clean pass resolved %d, want 1", res.Resolved)
	}
}

// Each checkout keeps its own open row for the same path. Before the open
// index carried worktree, the second insert hit the unique index and was
// dropped by INSERT OR IGNORE — one tree's contamination going unrecorded
// because another tree got there first (loto-nper).
func TestRecordViolations_TwoWorktreesEachHoldARowForOnePath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileScan(ctx, scanFrom("agent-b", gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA2}), liveEval()); err != nil {
		t.Fatal(err)
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("want one open row per worktree, got %d: %+v", len(open), open)
	}
	got := map[string]string{}
	for i := range open {
		got[open[i].Worktree] = open[i].Fingerprint
	}
	if got[""] != tcSHA1 || got["agent-b"] != tcSHA2 {
		t.Errorf("rows not attributed per worktree: %+v", got)
	}
}

// Admission's intersect is scoped too. Without it, agent-b's contamination
// would refuse the primary checkout's legitimate submit on a path whose
// content the primary never touched (loto-nper).
func TestUnresolvedViolationPaths_ScopedToTheAskingCheckout(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if _, err := s.ReconcileScan(ctx, scanFrom("agent-b", gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}
	primary, err := s.UnresolvedViolationPaths(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, blocked := primary[tcRogueGo]; blocked {
		t.Errorf("agent-b's violation blocks the primary checkout's submit: %+v", primary)
	}
	b, err := s.UnresolvedViolationPaths(ctx, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, blocked := b[tcRogueGo]; !blocked {
		t.Errorf("agent-b's own violation does not block agent-b: %+v", b)
	}
}

// A resolve that commits AFTER the scan read acks and BEFORE it inserts must
// still win: the operator was told the violation was cleared, and a row that
// reopens itself with no explanation is worse than no resolve at all (Codex
// #276 round 2, loto-njaj).
func TestRecordViolations_AckCommittedAfterTheSnapshotStillSuppressesTheInsert(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	obs := ObservedViolation{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, Baseline: tcBaseline, LeaseState: LeaseStateUnleased}

	// 1. The scan reads acks: nothing is acked for this path yet.
	acked, err := s.ackedFingerprints(ctx, []string{tcRogueGo}, tcBaseline, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(acked) != 0 {
		t.Fatalf("precondition: want an empty ack snapshot, got %+v", acked)
	}

	// 2. Concurrently, the operator records and resolves this exact content.
	rec, err := s.RecordViolations(ctx, []ObservedViolation{obs})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 {
		t.Fatalf("seed insert recorded %d rows, want 1", len(rec))
	}
	if err := s.ResolveViolation(ctx, rec[0].ID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}

	// 3. The scan proceeds on its stale view and tries to insert anyway.
	again, err := s.RecordViolations(ctx, []ObservedViolation{obs})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("stale ack snapshot reopened a resolved violation: %+v", again)
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("violation is open again after a successful resolve: %+v", open)
	}
}

// The re-check is keyed on content, not path: a DIFFERENT fingerprint on an
// acked path is a new mutation and must still be recorded.
func TestRecordViolations_AckReCheckDoesNotSwallowANewMutation(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	rec, err := s.RecordViolations(ctx, []ObservedViolation{
		{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, Baseline: tcBaseline, LeaseState: LeaseStateUnleased},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveViolation(ctx, rec[0].ID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}
	again, err := s.RecordViolations(ctx, []ObservedViolation{
		{PathCanonical: tcRogueGo, Fingerprint: tcSHA2, Baseline: tcBaseline, LeaseState: LeaseStateUnleased},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("new content on an acked path was swallowed: %+v", again)
	}
}

// A DB carrying the violations table's first shape gets the worktree column
// and the widened open-path index. The old index is not merely redundant:
// while it stands, two checkouts cannot each hold an open row for one path,
// so the second one's contamination is silently dropped (loto-nper).
func TestMigrate_ScopesTheOpenViolationIndexToWorktree(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// Rewind to the pre-worktree shape: the old index back, the new one gone.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_violations_open_path_wt`,
		`CREATE UNIQUE INDEX idx_violations_open_path ON violations(path_canonical) WHERE resolved_at IS NULL`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	pending, err := ensureViolationsOpenIndexScoped(ctx, s.db, false)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !pending {
		t.Fatal("probe says nothing to do on a pre-worktree index")
	}
	if _, err := ensureViolationsOpenIndexScoped(ctx, s.db, true); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var stale, scoped int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_violations_open_path'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_violations_open_path_wt'`).Scan(&scoped); err != nil {
		t.Fatal(err)
	}
	if stale != 0 || scoped != 1 {
		t.Fatalf("index state after migrate: stale=%d scoped=%d, want 0/1", stale, scoped)
	}
	// Re-probing is a no-op — the steady-state fast path depends on it.
	if pending, err := ensureViolationsOpenIndexScoped(ctx, s.db, false); err != nil || pending {
		t.Errorf("re-probe pending=%v err=%v, want false/nil", pending, err)
	}
}

// A violation recorded before rows carried a checkout must keep blocking
// EVERY checkout. Calling it "primary" would let a linked worktree upgrade
// straight past its own sticky violation — and if it has since leased the
// path, its own scan records nothing new, so the contaminated content
// submits clean (Codex #283 P1).
func TestMigrate_LegacyOpenViolationBlocksEveryCheckout(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// A row from before the column: no worktree, one open, one acked.
	rec, err := s.RecordViolations(ctx, []ObservedViolation{
		{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, Baseline: tcBaseline, LeaseState: LeaseStateUnleased},
		{PathCanonical: tcOtherGo, Fingerprint: tcSHA2, Baseline: tcBaseline, LeaseState: LeaseStateUnleased},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 2 {
		t.Fatalf("seed recorded %d rows, want 2", len(rec))
	}
	var ackedID string
	for i := range rec {
		if rec[i].PathCanonical == tcOtherGo {
			ackedID = rec[i].ID
		}
	}
	if err := s.ResolveViolation(ctx, ackedID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE violations SET worktree = ''`); err != nil {
		t.Fatalf("rewind to the pre-worktree shape: %v", err)
	}

	rewindToPreWorktreeShape(t, s)

	// Every checkout sees the open legacy row...
	for _, wt := range []string{"", "agent-b"} {
		open, err := s.UnresolvedViolationPaths(ctx, wt)
		if err != nil {
			t.Fatal(err)
		}
		if _, blocked := open[tcRogueGo]; !blocked {
			t.Errorf("legacy violation invisible to checkout %q: %+v", wt, open)
		}
	}
	// ...and no checkout's clean pass may close it. A clean reading here says
	// nothing about the tree the row actually came from, and clearing it on
	// that basis is the laundering this record exists to stop (Codex #283 P1).
	res, err := s.ReconcileScan(ctx, scanFrom("agent-b"), liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 0 {
		t.Errorf("a clean pass from agent-b resolved %d legacy rows, want 0", res.Resolved)
	}
	if res, err = s.ReconcileScan(ctx, scanOf(), liveEval()); err != nil {
		t.Fatal(err)
	}
	if res.Resolved != 0 {
		t.Errorf("a clean pass from the primary resolved %d legacy rows, want 0", res.Resolved)
	}
	// An explicit resolve is the only door, and it still works.
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("want the legacy row still open, got %+v", open)
	}
	if err := s.ResolveViolation(ctx, open[0].ID, "looked, it was reverted"); err != nil {
		t.Errorf("explicit resolve on a legacy row: %v", err)
	}
}

// A legacy acknowledgement clears nothing anywhere. Left at the column
// default it would read as an ack made in the primary checkout and suppress
// that tree's flags on content it never approved (Codex #283 P2).
func TestMigrate_LegacyAcknowledgementSuppressesNoCheckout(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	rec, err := s.RecordViolations(ctx, []ObservedViolation{
		{PathCanonical: tcRogueGo, Fingerprint: tcSHA1, Baseline: tcBaseline, LeaseState: LeaseStateUnleased},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveViolation(ctx, rec[0].ID, "legitimate, staying"); err != nil {
		t.Fatal(err)
	}
	rewindToPreWorktreeShape(t, s)

	var wt string
	if err := s.db.QueryRowContext(ctx, `SELECT worktree FROM violations WHERE id = ?`, rec[0].ID).Scan(&wt); err != nil {
		t.Fatal(err)
	}
	if wt != WorktreeLegacy {
		t.Errorf("resolved row backfilled to %q, want %q", wt, WorktreeLegacy)
	}
	// The same path and content, observed now from the primary checkout, is
	// flagged rather than waved through on that unattributable ack.
	res, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recorded) != 1 {
		t.Errorf("a legacy ack suppressed the primary checkout's flag: recorded=%d", len(res.Recorded))
	}
}

// rewindToPreWorktreeShape puts an already-migrated table back into its
// pre-worktree shape — index first, since the column lives inside it — and
// then drives migrate's two steps in their real order. Rewinding rather than
// hand-building a legacy DB keeps the test honest about what migrate does to
// rows that are already there.
func rewindToPreWorktreeShape(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_violations_open_path_wt`,
		`ALTER TABLE violations DROP COLUMN worktree`,
		`CREATE UNIQUE INDEX idx_violations_open_path ON violations(path_canonical) WHERE resolved_at IS NULL`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if pending, err := ensureViolationsWorktree(ctx, s.db, false); err != nil || !pending {
		t.Fatalf("probe on a pre-worktree table: pending=%v err=%v", pending, err)
	}
	for _, step := range []func(context.Context, sqlExecQuerier, bool) (bool, error){
		ensureViolationsWorktree, ensureViolationsOpenIndexScoped,
	} {
		if _, err := step(ctx, s.db, true); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
}

// migrate runs schema.sql ahead of every ensure, so a re-migrate must not
// leave the legacy index standing beside the scoped one. Two live constraints
// mean RecordViolations' INSERT OR IGNORE silently eats the second checkout's
// open row for a shared path — the nper hole, re-armed by a later migration
// (Codex #283 P1).
func TestMigrate_ReMigrateDoesNotResurrectTheLegacyIndex(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// The base schema must not declare the unscoped index at all.
	if strings.Contains(schemaSQL, "idx_violations_open_path\n") ||
		strings.Contains(schemaSQL, "idx_violations_open_path ") {
		t.Error("schema.sql still declares the unscoped open-violation index")
	}

	// Simulate the crash window: the schema tx committed, user_version never
	// landed, and something put the legacy index back.
	if _, err := s.db.ExecContext(ctx,
		`CREATE UNIQUE INDEX idx_violations_open_path ON violations(path_canonical) WHERE resolved_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	pending, err := ensureViolationsOpenIndexScoped(ctx, s.db, false)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("probe reports nothing to do while the legacy index is live beside the scoped one")
	}
	if _, err := ensureViolationsOpenIndexScoped(ctx, s.db, true); err != nil {
		t.Fatal(err)
	}
	if got, err := indexExists(ctx, s.db, "idx_violations_open_path"); err != nil || got {
		t.Fatalf("legacy index survived: exists=%v err=%v", got, err)
	}

	// And the property it was breaking holds again.
	if _, err := s.ReconcileScan(ctx, scanOf(gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA1}), liveEval()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileScan(ctx, scanFrom("agent-b", gate.Observation{Path: tcRogueGo, Fingerprint: tcSHA2}), liveEval()); err != nil {
		t.Fatal(err)
	}
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("want one open row per checkout, got %d: %+v", len(open), open)
	}
}
