package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestAcquireLocks_EmitsLockAcquiredEvent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	events, err := s.EventsForTarget(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventKind(events, EventLockAcquired) {
		t.Fatalf("expected lock_acquired event, got %+v", events)
	}
	for _, e := range events {
		if e.Kind == EventLockAcquired {
			if e.ActorUUID != tcAlice {
				t.Errorf("ActorUUID = %q, want %q", e.ActorUUID, tcAlice)
			}
		}
	}
}

func TestReleaseLocks_EmitsLockReleasedEvent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice); err != nil {
		t.Fatal(err)
	}

	events, err := s.EventsForTarget(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventKind(events, EventLockReleased) {
		t.Fatalf("expected lock_released event, got %+v", events)
	}
	for _, e := range events {
		if e.Kind == EventLockReleased {
			if e.ActorUUID != tcAlice {
				t.Errorf("ActorUUID = %q, want %q", e.ActorUUID, tcAlice)
			}
		}
	}
}

func TestReleaseLocks_NoEventForNonOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	// Bob tries to release alice's lock — should be rejected.
	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].State != StateNotOwner {
		t.Fatalf("expected StateNotOwner, got %v", res[0].State)
	}

	events, err := s.EventsForTarget(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if containsEventKind(events, EventLockReleased) {
		t.Fatalf("no lock_released event should exist for non-owner release, got %+v", events)
	}
}

func containsEventKind(events []domain.Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
