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
