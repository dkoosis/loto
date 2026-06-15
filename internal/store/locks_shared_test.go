package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"loto/internal/domain"
)

// liveProbe reports every pid alive — keeps seeded holders non-stale so the
// mode predicate (not reclaim) governs coexistence.
func liveProbe(string, int, int64) bool { return true }

// peerOn clones base onto a different owner, preserving the same on-disk target
// so two records contend on one file. Mode is set explicitly by the caller.
func peerOn(base domain.LockRecord, owner, mode string) domain.LockRecord {
	p := base
	p.OwnerUUID, p.SessionUUID = owner, owner
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
			holders[r.OwnerUUID] = true
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

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, tcAlice)
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

func TestAcquire_SharedDoesNotStripWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("shared acquire: %v", err)
	}
	fi, err := os.Stat(rec.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("shared lock must NOT strip owner-write bit; perm=%v", fi.Mode().Perm())
	}
}

func TestAcquire_ExclusiveStripsWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("exclusive acquire: %v", err)
	}
	fi, err := os.Stat(rec.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("exclusive lock must strip owner-write bit; perm=%v", fi.Mode().Perm())
	}
}

// TestBreakLocks_SharedDoesNotRestoreWriteBit guards the break-side restore
// guard (loto-o09s): two shared holders on a deliberately read-only file;
// breaking one holder must NOT flip the file writable (shared never stripped
// the bit — restoring would spuriously grant owner-write while the survivor's
// shared lock still stands) and the surviving holder's row must stay intact.
func TestBreakLocks_SharedDoesNotRestoreWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)
	if err := os.Chmod(a.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatalf("alice shared acquire: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatalf("bob shared acquire: %v", err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{a.Target}, "carol", BreakForce, "test break", "h", liveProbe)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err != nil {
		t.Fatalf("break should succeed, got Err=%v", res[0].Err)
	}
	if res[0].RestoreErr != nil {
		t.Fatalf("no restore should be attempted on a shared break, got RestoreErr=%v", res[0].RestoreErr)
	}

	fi, err := os.Stat(a.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("breaking a shared holder must leave file mode unchanged; want 444, got %o", fi.Mode().Perm())
	}

	// A forced break removes EVERY holder of the target (loto-w77f); the
	// write-bit must still be left untouched (shared never stripped it).
	rows, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var survivors []domain.LockRecord
	for _, r := range rows {
		if r.Target.Canonical == a.Target.Canonical {
			survivors = append(survivors, r)
		}
	}
	if len(survivors) != 0 {
		t.Fatalf("forced break must remove all shared holders, got %d survivors: %+v", len(survivors), survivors)
	}
}

// TestRelease_SharedDoesNotRestoreWriteBit guards the release-side guard: a
// shared release never stripped the bit, so restore must be skipped (restoring
// would spuriously ADD owner-write). Start the file read-only; a shared
// acquire leaves it untouched, and release must NOT flip it writable.
func TestRelease_SharedDoesNotRestoreWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if err := os.Chmod(rec.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("shared acquire: %v", err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{rec.Target}, tcAlice); err != nil {
		t.Fatalf("release: %v", err)
	}
	fi, err := os.Stat(rec.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("shared release must NOT restore owner-write; perm=%v", fi.Mode().Perm())
	}
}

// deadProbe reports every pid dead — makes seeded holders stale so a peer's
// acquire reclaims them despite a live TTL.
func deadProbe(string, int, int64) bool { return false }

// TestAcquire_SharedReclaimRestoresWriteBit guards the acquire-reclaim restore
// (loto-22ka): a stale EXCLUSIVE holder left the file write-stripped (0o444);
// a SHARED acquirer reclaims the stale row but never re-strips, so the acquire
// must restore owner-write. Without the restore the row state says advisory
// shared lock while the inode stays read-only, and no release/break/downgrade
// of the shared lock will ever flip it back.
func TestAcquire_SharedReclaimRestoresWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	stale := mkFileLock(t, "a.go", tcAlice, time.Hour)
	stale.Mode = domain.ModeExclusive
	stale.PID = 4242 // durable pid; deadProbe will report it dead
	stale.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatalf("seed stale exclusive: %v", err)
	}
	fi, err := os.Stat(stale.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("precondition: exclusive acquire must strip owner-write; perm=%v", fi.Mode().Perm())
	}

	bob := peerOn(stale, tcBob, domain.ModeShared)
	bob.PID = 5555
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, deadProbe); err != nil {
		t.Fatalf("bob shared acquire over stale exclusive must succeed: %v", err)
	}

	fi, err = os.Stat(stale.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("shared acquire reclaiming stale exclusive must restore owner-write; perm=%v", fi.Mode().Perm())
	}
}

// TestAcquire_MixedBatchReclaimRestoresOnlySharedTargets is the mixed-batch
// variant (loto-22ka): one batch acquires SHARED over a stale-exclusive holder
// on a.go and EXCLUSIVE over a stale-exclusive holder on b.go. The reclaim
// restore must re-add owner-write on a.go (shared acquirer never re-strips)
// but must NOT undo the acquirer's own re-strip on b.go.
func TestAcquire_MixedBatchReclaimRestoresOnlySharedTargets(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	staleA := mkFileLock(t, "a.go", tcAlice, time.Hour)
	staleA.Mode = domain.ModeExclusive
	staleA.PID = 4242
	staleA.ProcStart = 9999
	staleB := mkFileLock(t, "b.go", tcAlice, time.Hour)
	staleB.Mode = domain.ModeExclusive
	staleB.PID = 4242
	staleB.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{staleA, staleB}, liveProbe); err != nil {
		t.Fatalf("seed stale exclusive holders: %v", err)
	}

	bobShared := peerOn(staleA, tcBob, domain.ModeShared)
	bobShared.PID = 5555
	bobExcl := peerOn(staleB, tcBob, domain.ModeExclusive)
	bobExcl.PID = 5555
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bobShared, bobExcl}, deadProbe); err != nil {
		t.Fatalf("bob mixed-batch acquire over stale holders must succeed: %v", err)
	}

	fiA, err := os.Stat(staleA.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fiA.Mode().Perm()&0o200 == 0 {
		t.Errorf("shared-acquired a.go must end writable after reclaim; perm=%v", fiA.Mode().Perm())
	}
	fiB, err := os.Stat(staleB.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fiB.Mode().Perm()&0o200 != 0 {
		t.Errorf("exclusive-acquired b.go must stay write-stripped (acquirer re-stripped); perm=%v", fiB.Mode().Perm())
	}
}

// TestAcquire_ReclaimStaleSharedDoesNotRestoreWriteBit guards the mode guard
// on the reclaim restore (shouldRestoreOwnerWrite, loto-o09s): a stale SHARED
// holder never stripped owner-write, so reclaiming it must NOT flip a
// deliberately read-only file writable.
func TestAcquire_ReclaimStaleSharedDoesNotRestoreWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	stale := mkFileLock(t, "a.go", tcAlice, time.Hour)
	stale.Mode = domain.ModeShared
	stale.PID = 4242
	stale.ProcStart = 9999
	if err := os.Chmod(stale.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatalf("seed stale shared: %v", err)
	}

	bob := peerOn(stale, tcBob, domain.ModeShared)
	bob.PID = 5555
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, deadProbe); err != nil {
		t.Fatalf("bob shared acquire over stale shared must succeed: %v", err)
	}

	fi, err := os.Stat(stale.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("reclaiming a stale SHARED row must leave file mode unchanged; want 444, got %o", fi.Mode().Perm())
	}
}

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

	res, err := s.BreakLocks(ctx, []domain.Target{a.Target}, "carol", BreakForce, "test break", "h", liveProbe)
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
