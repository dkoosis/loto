package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

// peerOn clones base onto a different owner, preserving the same on-disk target
// so two records contend on one file. Mode is set explicitly by the caller.
func peerOn(base domain.LockRecord, owner, mode string) domain.LockRecord {
	p := base
	p.OwnerUUID, p.SessionUUID = domain.AgentUUID(owner), domain.SessionUUID(owner)
	p.Mode = mode
	return p
}

func TestAcquire_SharedSharedCoexist(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice shared acquire: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatalf("bob shared acquire should succeed (shared+shared): %v", err)
	}
	rows, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 coexisting shared rows, got %d", len(rows))
	}
}

func TestAcquire_ExclusiveBlocksShared(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeExclusive
	b := peerOn(a, tcBob, domain.ModeShared)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice exclusive: %v", err)
	}
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe)
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("want MultiConflictError (exclusive blocks shared), got %v", err)
	}
}

// TestLockForOwnerAt_MultiHolderUnambiguous pins the composite-PK regression
// guard (loto-k5el.2 T5.5): with two shared holders on one target, LockForOwnerAt
// returns the RIGHT owner's row for each, and ListLocks surfaces both. Guards
// against re-introducing the single-row-per-target assumption.
func TestLockForOwnerAt_MultiHolderUnambiguous(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatalf("bob: %v", err)
	}

	la, err := s.LockForOwnerAt(ctx, a.Target, tcAlice)
	if err != nil || la == nil || la.OwnerUUID != tcAlice {
		t.Fatalf("LockForOwnerAt(alice) = %v, err=%v; want alice's row", la, err)
	}
	lb, err := s.LockForOwnerAt(ctx, a.Target, tcBob)
	if err != nil || lb == nil || lb.OwnerUUID != tcBob {
		t.Fatalf("LockForOwnerAt(bob) = %v, err=%v; want bob's row", lb, err)
	}

	rows, _ := s.ListLocks(ctx)
	holders := map[string]bool{}
	for _, r := range rows {
		if r.Target.Canonical == a.Target.Canonical {
			holders[string(r.OwnerUUID)] = true
		}
	}
	if !holders[tcAlice] || !holders[tcBob] {
		t.Fatalf("ListLocks must surface both shared holders, got %v", holders)
	}
}

// TestRelease_MultiHolderEachReleasesOwn guards the multi-holder release fix
// (loto-k5el.2): two shared holders on one target; each must be able to release
// its OWN row without the other's row shadowing it into a not-owner misclassify.
func TestRelease_MultiHolderEachReleasesOwn(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatalf("bob: %v", err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, tcAlice, liveProbe)
	if err != nil {
		t.Fatalf("alice release: %v", err)
	}
	if len(res) != 1 || res[0].State != StateUnlocked {
		t.Fatalf("alice must unlock her own shared row, got %+v", res)
	}
	// Alice's row gone, bob's row survives.
	if la, _ := s.LockForOwnerAt(ctx, a.Target, tcAlice); la != nil {
		t.Fatalf("alice's row should be deleted, got %+v", la)
	}
	if lb, _ := s.LockForOwnerAt(ctx, a.Target, tcBob); lb == nil {
		t.Fatalf("bob's shared row must survive alice's release")
	}
}

// TestBreakLocks_SharedDoesNotRestoreWriteBit guards the break-side restore
// guard (loto-o09s): two shared holders on a deliberately read-only file;
// breaking one holder must NOT flip the file writable (shared never stripped
// the bit — restoring would spuriously grant owner-write while the survivor's
// shared lock still stands) and the surviving holder's row must stay intact.
// TestRelease_SharedDoesNotRestoreWriteBit guards the release-side guard: a
// shared release never stripped the bit, so restore must be skipped (restoring
// would spuriously ADD owner-write). Start the file read-only; a shared
// acquire leaves it untouched, and release must NOT flip it writable.
// a SHARED acquirer reclaims the stale row but never re-strips, so the acquire
// must restore owner-write. Without the restore the row state says advisory
// shared lock while the inode stays read-only, and no release/break/downgrade
// of the shared lock will ever flip it back.
// TestAcquire_MixedBatchReclaimRestoresOnlySharedTargets is the mixed-batch
// variant (loto-22ka): one batch acquires SHARED over a stale-exclusive holder
// on a.go and EXCLUSIVE over a stale-exclusive holder on b.go. The reclaim
// restore must re-add owner-write on a.go (shared acquirer never re-strips)
// but must NOT undo the acquirer's own re-strip on b.go.
// TestAcquire_ReclaimStaleSharedDoesNotRestoreWriteBit guards the mode guard
// on the reclaim path (loto-o09s): a stale SHARED
// holder never stripped owner-write, so reclaiming it must NOT flip a
// deliberately read-only file writable.
// TestBreakLocks_MultiHolderShared is the loto-w77f regression: a target held
// shared by two agents must lose BOTH holders on a forced break, with one
// lock_broken event per holder naming the right subject. Before the fix
// loadLocksByTargetTx keyed its result by target_canonical alone, collapsing
// the holders to one arbitrary survivor — the break reported success while a
// blocker silently remained.
func TestBreakLocks_MultiHolderShared(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice shared acquire: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatalf("bob shared acquire: %v", err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{a.Target}, "carol", BreakForce, "test break", liveProbe)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err != nil {
		t.Fatalf("break should succeed, got Err=%v", res[0].Err)
	}

	// No holder may survive.
	for _, r := range mustListLocks(ctx, t, s) {
		if r.Target.Canonical == a.Target.Canonical {
			t.Fatalf("holder survived multi-holder break: %+v", r)
		}
	}

	// One lock_broken event per holder, each naming the broken owner.
	events, err := s.EventsForTarget(ctx, a.Target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	subjects := map[string]int{}
	for _, e := range events {
		if e.Kind == EventLockBroken {
			subjects[e.SubjectUUID]++
		}
	}
	if subjects[tcAlice] != 1 || subjects[tcBob] != 1 {
		t.Fatalf("want one lock_broken per holder (alice=1 bob=1), got %v in %+v", subjects, events)
	}
}

func mustListLocks(ctx context.Context, t *testing.T, s *Store) []domain.LockRecord {
	t.Helper()
	rows, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return rows
}

// TestAcquire_HoldsFlockDuringRestoreChmod encodes the loto-v8ch correctness
// contract, which SUPERSEDES the prior loto-9uy5 posture (a P3 perf concern with
// no correctness loss). The chmod half of the post-commit reclaim-restore MUST
// run while the op-flock is held: otherwise a peer can take the flock mid-restore
// and either observe a torn row+file view (DB says reclaimed, file still
// read-only) or acquire exclusive + re-strip, after which the lagging restore
// re-adds owner-write under the peer's lease and defeats its exclusivity. This is
// the same silent-clobber Break/Doctor hold the flock to prevent (loto-4qt).
//
// loto-9uy5's legitimate anti-stall goal — keep the detached AUDIT's write tx off
// the flock — is preserved structurally: AcquireLocks emits the
// mode_restore_failed events AFTER flock.release(), so only the bounded fchmod
// runs under the flock, never the audit's beginTx. This test pins the chmod-held
// half; the audit-off-flock half is enforced by source ordering (restore returns
// events; caller releases the flock before appendAuditDetached).
