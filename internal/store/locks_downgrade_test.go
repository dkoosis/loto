package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestDowngrade_ExclusiveToShared_RestoresWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected stripped before downgrade")
	}
	if err := s.DowngradeLock(ctx, rec.Target, tcAlice); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	l, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if l == nil || l.EffectiveMode() != domain.ModeShared {
		t.Fatalf("want shared after downgrade, got %v", l)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("downgrade must restore owner-write; perm=%v", fi.Mode().Perm())
	}
}

func TestDowngrade_NoLock_Errors(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour) // file exists, no lock
	err := s.DowngradeLock(ctx, rec.Target, tcAlice)
	if !errors.Is(err, ErrNoLockAtTarget) {
		t.Fatalf("want ErrNoLockAtTarget, got %v", err)
	}
}

func TestDowngrade_AlreadyShared_NoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.DowngradeLock(ctx, rec.Target, tcAlice); err != nil {
		t.Fatalf("downgrade of already-shared should be a no-op, got %v", err)
	}
}
