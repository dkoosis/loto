package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

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

	if _, err := s.AcquireLock(ctx, mkLock("internal/store/", "alice", time.Hour), live); err != nil {
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
	if len(conflict.Blockers) != 1 || conflict.Blockers[0].OwnerUUID != "alice" {
		t.Fatalf("expected single blocker alice; got %+v", conflict.Blockers)
	}
}

func TestAcquireSameAgentRefreshes(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }

	first := mkLock("a.go", "alice", time.Hour)
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
