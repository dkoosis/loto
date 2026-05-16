package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestEvents_InsertAndReadBackAllKinds(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt := domain.Target{Canonical: tcAGo}
	now := time.Now()

	kinds := []string{EventLockAcquired, EventLockReleased, EventLockBroken, EventLockReclaimedStale}
	for i, k := range kinds {
		_, err := s.AppendEvent(ctx, domain.Event{
			Target:      tgt,
			Kind:        k,
			ActorUUID:   tcAlice,
			SubjectUUID: tcBob,
			Reason:      "r-" + k,
			CreatedAt:   now.Add(time.Duration(i) * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("AppendEvent(%s): %v", k, err)
		}
	}

	got, err := s.EventsForTarget(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(kinds) {
		t.Fatalf("got %d events, want %d", len(got), len(kinds))
	}
	for i, e := range got {
		if e.Kind != kinds[i] {
			t.Errorf("event[%d].Kind=%s want %s", i, e.Kind, kinds[i])
		}
		if e.ActorUUID != tcAlice || e.SubjectUUID != tcBob {
			t.Errorf("event[%d] actor/subject mismatch: %+v", i, e)
		}
	}

	all, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(kinds) {
		t.Fatalf("ListEvents got %d want %d", len(all), len(kinds))
	}
}

func TestAppendEvents_BatchInsert(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt := domain.Target{Canonical: tcAGo}
	now := time.Now()

	evs := []domain.Event{
		{Target: tgt, Kind: EventLockAcquired, ActorUUID: tcAlice, Reason: "r1", CreatedAt: now},
		{Target: tgt, Kind: EventLockReleased, ActorUUID: tcAlice, Reason: "r2", CreatedAt: now.Add(time.Millisecond)},
		{Target: tgt, Kind: EventLockBroken, ActorUUID: tcBob, SubjectUUID: tcAlice, Reason: "r3", CreatedAt: now.Add(2 * time.Millisecond)},
	}
	if err := s.AppendEvents(ctx, evs); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	got, err := s.EventsForTarget(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(evs) {
		t.Fatalf("got %d events, want %d", len(got), len(evs))
	}
}

func TestAppendEvents_EmptySliceNoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.AppendEvents(ctx, nil); err != nil {
		t.Fatalf("AppendEvents(nil): %v", err)
	}
	if err := s.AppendEvents(ctx, []domain.Event{}); err != nil {
		t.Fatalf("AppendEvents([]): %v", err)
	}
	all, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 events, got %d", len(all))
	}
}

func TestAppendEvents_AssignsIDs(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	evs := []domain.Event{
		{Target: domain.Target{Canonical: tcAGo}, Kind: EventLockAcquired, ActorUUID: tcAlice, CreatedAt: time.Now()},
		{Target: domain.Target{Canonical: "b.go"}, Kind: EventLockAcquired, ActorUUID: tcAlice, CreatedAt: time.Now()},
	}
	if err := s.AppendEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		if e.ID == "" {
			t.Errorf("evs[%d].ID empty after AppendEvents", i)
		}
	}
}

func TestEvents_ModeRestoreFailedAccepted(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_, err := s.AppendEvent(ctx, domain.Event{
		Target:    domain.Target{Canonical: tcXGo},
		Kind:      EventModeRestoreFailed,
		ActorUUID: tcAlice,
		Reason:    "EPERM",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendEvent mode_restore_failed: %v", err)
	}
}
