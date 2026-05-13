package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// Cap by count: > 1000 events present, rotate trims to exactly 1000 (newest).
func TestRotateEvents_CapByCount(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	const N = 1100
	for i := range N {
		_, err := s.AppendEvent(ctx, domain.Event{
			Target:    domain.Target{Canonical: tcXGo},
			Kind:      EventLockAcquired,
			ActorUUID: tcAlice,
			Reason:    "r",
			CreatedAt: base.Add(time.Duration(i) * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
	}

	if err := s.RotateEvents(ctx); err != nil {
		t.Fatalf("RotateEvents: %v", err)
	}

	got, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != eventsRetentionMax {
		t.Fatalf("got %d events, want %d", len(got), eventsRetentionMax)
	}
	// Surviving rows must be the newest — first surviving row at base+100ms.
	want := base.Add(100 * time.Millisecond).UnixNano()
	if got[0].CreatedAt.UnixNano() != want {
		t.Errorf("oldest surviving event=%d, want %d", got[0].CreatedAt.UnixNano(), want)
	}
}

// Cap by age: events older than 7 days are deleted regardless of count.
func TestRotateEvents_CapByAge(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-eventsRetentionAge - time.Hour)
	young := now.Add(-time.Hour)

	for i, when := range []time.Time{old, old.Add(time.Second), young, young.Add(time.Second)} {
		_, err := s.AppendEvent(ctx, domain.Event{
			Target:    domain.Target{Canonical: tcXGo},
			Kind:      EventLockAcquired,
			ActorUUID: tcAlice,
			Reason:    "r",
			CreatedAt: when.Add(time.Duration(i) * time.Nanosecond),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := s.RotateEvents(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	for _, e := range got {
		if now.Sub(e.CreatedAt) > eventsRetentionAge {
			t.Errorf("event %s survived past retention age: %v", e.ID, e.CreatedAt)
		}
	}
}
