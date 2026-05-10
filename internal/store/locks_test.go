package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestReleaseLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	l := mkLock("a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLock(ctx, l, live); err != nil {
		t.Fatal(err)
	}

	if err := s.ReleaseLock(ctx, l.Target, "bob"); err == nil {
		t.Fatal("non-owner release must fail")
	}
	if err := s.ReleaseLock(ctx, l.Target, tcAlice); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("lock should be gone, got %+v", got)
	}
}

func TestBreakLockStaleOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	l := mkLock("a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLock(ctx, l, live); err != nil {
		t.Fatal(err)
	}

	if err := s.BreakLock(ctx, l.Target, "bob", false, "test", live); err == nil {
		t.Fatal("live break without force must fail")
	}
	if err := s.BreakLock(ctx, l.Target, "bob", true, "deadline", live); err != nil {
		t.Fatalf("force break: %v", err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("lock should be gone, got %+v", got)
	}
	tags, _ := s.TagsOnTarget(ctx, l.Target)
	if len(tags) != 1 || tags[0].Event != "lock_broken" {
		t.Fatalf("expected single lock_broken tag, got %+v", tags)
	}
}

func mustOpen(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkLock(target, agent string, expIn time.Duration) domain.LockRecord {
	tgt, _ := domain.Canonicalize(target)
	now := time.Now()
	return domain.LockRecord{
		Target: tgt, OwnerUUID: agent, SessionUUID: agent,
		Intent: "test", CreatedAt: now, ExpiresAt: now.Add(expIn),
		Host: "h", PID: 1,
	}
}

func TestAcquireOverlapBlocks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }

	if _, err := s.AcquireLock(ctx, mkLock("internal/store/", tcAlice, time.Hour), live); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}
	res, err := s.AcquireLock(ctx, mkLock("internal/store/store.go", "bob", time.Hour), live)
	if err == nil {
		t.Fatalf("expected conflict; got result %+v", res)
	}
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *ConflictError; got %T", err)
	}
	if len(conflict.Blockers) != 1 || conflict.Blockers[0].OwnerUUID != tcAlice {
		t.Fatalf("expected single blocker alice; got %+v", conflict.Blockers)
	}
}

func TestAcquireSameAgentRefreshes(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }

	first := mkLock("a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLock(ctx, first, live); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Intent = "refreshed"
	second.ExpiresAt = first.ExpiresAt.Add(time.Hour)
	if _, err := s.AcquireLock(ctx, second, live); err != nil {
		t.Fatalf("refresh must succeed: %v", err)
	}
	got, err := s.LockAt(ctx, second.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Intent != "refreshed" {
		t.Fatalf("refresh did not update; got %+v", got)
	}
}
