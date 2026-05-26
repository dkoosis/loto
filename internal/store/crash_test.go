package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestCrash_AcquireConflictNoPartialRow — a conflicting AcquireLocks returns
// *MultiConflictError without writing anything.
func TestCrash_AcquireConflictNoPartialRow(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	now := time.Now()

	aliceLock := domain.LockRecord{
		Target:    domain.Target{Canonical: a},
		OwnerUUID: tcAlice, SessionUUID: tcAlice,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1,
	}
	bobLock := aliceLock
	bobLock.OwnerUUID = tcBob
	bobLock.SessionUUID = tcBob

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{aliceLock}, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bobLock}, live); err == nil {
		t.Fatal("expected conflict")
	}
	all, _ := s.ListLocks(ctx)
	for _, l := range all {
		if l.OwnerUUID == tcBob {
			t.Errorf("bob should have no lock row after conflict; got %+v", l)
		}
	}
}

// TestCrash_BreakLockAtomic — the event row and the lock deletion both happen
// or neither does. Force a conflict (no force, live lock) and assert the row is
// unchanged and no event was written.
func TestCrash_BreakLockAtomic(t *testing.T) {
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "x", "h", live)
	if err != nil || res[0].Err == nil {
		t.Fatal("expected break-without-force on live lock to fail")
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got == nil || got.OwnerUUID != tcAlice {
		t.Fatalf("lock should still belong to alice; got %+v", got)
	}
	events, _ := s.EventsForTarget(ctx, l.Target)
	for _, e := range events {
		if e.Kind == EventLockBroken || e.Kind == EventLockReclaimedStale {
			t.Errorf("no break/reclaim event should have been written; got %+v", events)
			break
		}
	}
}
