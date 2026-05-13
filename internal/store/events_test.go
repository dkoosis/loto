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
	tgt := domain.Target{Canonical: "a.go"}
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
