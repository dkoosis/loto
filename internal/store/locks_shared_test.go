package store

import (
	"context"
	"errors"
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
