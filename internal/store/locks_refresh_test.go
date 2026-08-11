package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestRefreshLocks_ExtendsOwnLease is the ccp-z1vj.6 refresh half: a live
// holder pushes its TTL out before expiry, in place — same row, same
// created_at, no unlock/relock.
func TestRefreshLocks_ExtendsOwnLease(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, 2*time.Second)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before, err := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if err != nil || before == nil {
		t.Fatalf("read back: %v %v", before, err)
	}

	results, err := s.RefreshLocks(ctx, []domain.Target{rec.Target}, tcAlice, time.Hour)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("want one clean result, got %+v", results)
	}
	after, err := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if err != nil || after == nil {
		t.Fatalf("read back after: %v %v", after, err)
	}
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("expires_at not extended: %v !> %v", after.ExpiresAt, before.ExpiresAt)
	}
	if !after.ExpiresAt.Equal(results[0].ExpiresAt) {
		t.Errorf("result ExpiresAt=%v disagrees with row %v", results[0].ExpiresAt, after.ExpiresAt)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("refresh must not restart the hold: created_at %v → %v", before.CreatedAt, after.CreatedAt)
	}
}

// TestRefreshLocks_NotHeld covers both flavors of "you don't hold this":
// no row at all, and a row owned by a peer. Neither may extend anything.
func TestRefreshLocks_NotHeld(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	t.Run("no lock at target", func(t *testing.T) {
		rec := mkFileLock(t, "a.go", tcAlice, time.Hour) // file exists, never locked
		results, err := s.RefreshLocks(ctx, []domain.Target{rec.Target}, tcAlice, time.Hour)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(results) != 1 || !errors.Is(results[0].Err, ErrNoLockAtTarget) {
			t.Fatalf("want ErrNoLockAtTarget, got %+v", results)
		}
	})

	t.Run("peer holds it", func(t *testing.T) {
		rec := mkFileLock(t, "b.go", tcAlice, time.Hour)
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		before, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
		results, err := s.RefreshLocks(ctx, []domain.Target{rec.Target}, tcBob, time.Hour)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if len(results) != 1 || !errors.Is(results[0].Err, ErrNoLockAtTarget) {
			t.Fatalf("bob must not refresh alice's lock, got %+v", results)
		}
		after, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
		if !after.ExpiresAt.Equal(before.ExpiresAt) {
			t.Errorf("peer refresh moved alice's lease: %v → %v", before.ExpiresAt, after.ExpiresAt)
		}
	})
}

// TestRefreshLocks_ExpiredLeaseRefused pins the reclamation guarantee: once the
// TTL has lapsed the territory is any peer's to take, so a late refresh must
// NOT resurrect it. The holder's remedy is to re-acquire.
func TestRefreshLocks_ExpiredLeaseRefused(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.ExpiresAt = time.Now().Add(-time.Minute) // already lapsed
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	results, err := s.RefreshLocks(ctx, []domain.Target{rec.Target}, tcAlice, time.Hour)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(results) != 1 || !errors.Is(results[0].Err, ErrLeaseExpired) {
		t.Fatalf("want ErrLeaseExpired, got %+v", results)
	}
	after, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if after.ExpiresAt.After(time.Now()) {
		t.Errorf("expired lease was resurrected: expires_at=%v", after.ExpiresAt)
	}
}

// TestRefreshLocks_AuditsRefresh keeps refresh inside the audit trail every
// other lock mutator writes to (NORTH_STAR: lock-row mutations are auditable).
func TestRefreshLocks_AuditsRefresh(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := s.RefreshLocks(ctx, []domain.Target{rec.Target}, tcAlice, 2*time.Hour); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	evs, err := s.EventsForTarget(ctx, rec.Target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if e.Kind == EventLockRefreshed && e.ActorUUID == tcAlice {
			return
		}
	}
	t.Fatalf("no %s event for %s in %+v", EventLockRefreshed, tcAlice, evs)
}

// TestRefreshLocks_EmptyTargets is the batch-API parity case: no targets, no
// flock, no tx, empty slice (mirrors ReleaseLocks/DowngradeLocks).
func TestRefreshLocks_EmptyTargets(t *testing.T) {
	s := mustOpen(t)
	results, err := s.RefreshLocks(context.Background(), nil, tcAlice, time.Hour)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want empty results, got %+v", results)
	}
}
