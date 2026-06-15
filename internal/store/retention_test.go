package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// rotateEventsNow drives the same-package rotateEventsTx in its own committed
// tx — the production seam exercised by AcquireLocks and `doctor --repair`.
// Replaces the former test-only Store.RotateEvents export (loto-qnz8).
func rotateEventsNow(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}
	defer cleanup()
	if err := rotateEventsTx(ctx, tx, time.Now()); err != nil {
		t.Fatalf("rotateEventsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

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

	rotateEventsNow(ctx, t, s)

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

// Cap by count with ties: when multiple events share the same created_at,
// the tie-breaker must be deterministic (insertion order / rowid), not random
// UUID order. Layout: 10 tie-group events at a low timestamp, then 995
// unique-timestamp events that are all newer. Total = 1005, retention = 1000,
// so 5 of the 10 tie-group members must be pruned. With rowid tiebreaker,
// the 5 earliest-inserted (lowest rowid) are pruned; the 5 latest survive.
func TestRotateEvents_CapByCount_TieDeterminism(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	const (
		tieGroup = 10
		newer    = eventsRetentionMax - 5 // 995 newer events → 995+10 = 1005 total
	)
	tieTS := base // all tie-group events share this timestamp

	// Phase 1: insert tie-group events (indices 0..9), all at tieTS.
	tieIDs := make([]string, 0, tieGroup)
	for i := range tieGroup {
		_ = i
		id, err := s.AppendEvent(ctx, domain.Event{
			Target:    domain.Target{Canonical: tcXGo},
			Kind:      EventLockAcquired,
			ActorUUID: tcAlice,
			Reason:    "r",
			CreatedAt: tieTS,
		})
		if err != nil {
			t.Fatalf("AppendEvent tie[%d]: %v", i, err)
		}
		tieIDs = append(tieIDs, id)
	}

	// Phase 2: insert 995 events with strictly increasing timestamps after tieTS.
	for i := range newer {
		_, err := s.AppendEvent(ctx, domain.Event{
			Target:    domain.Target{Canonical: tcXGo},
			Kind:      EventLockAcquired,
			ActorUUID: tcAlice,
			Reason:    "r",
			CreatedAt: tieTS.Add(time.Duration(i+1) * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("AppendEvent newer[%d]: %v", i, err)
		}
	}

	rotateEventsNow(ctx, t, s)

	got, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != eventsRetentionMax {
		t.Fatalf("got %d events, want %d", len(got), eventsRetentionMax)
	}

	// The 5 tie-group events that must survive are the last 5 inserted
	// (highest rowid). The first 5 (lowest rowid) must be pruned.
	wantSurvivors := make(map[string]bool, 5)
	for _, id := range tieIDs[5:] {
		wantSurvivors[id] = true
	}
	wantPruned := make(map[string]bool, 5)
	for _, id := range tieIDs[:5] {
		wantPruned[id] = true
	}

	gotIDs := make(map[string]bool, len(got))
	for _, e := range got {
		gotIDs[e.ID] = true
	}
	for id := range wantSurvivors {
		if !gotIDs[id] {
			t.Errorf("expected survivor %s (late rowid in tie group) was pruned", id)
		}
	}
	for id := range wantPruned {
		if gotIDs[id] {
			t.Errorf("expected pruned %s (early rowid in tie group) survived", id)
		}
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

	rotateEventsNow(ctx, t, s)

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

// loto-bvdk: rotation must also fire on the release/break/downgrade append
// paths, not just acquire/doctor. Each subtest seeds >cap events AFTER the
// lock exists (so no acquire rotates them away), then drives the op under
// test and asserts ListEvents trimmed to the cap — proving the op itself
// rotated. Without the fix these tables grow unbounded on acquire-rare,
// mutate-frequent workloads.
func TestRotateEvents_FiresOnReleaseBreakDowngrade(t *testing.T) {
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	// seedExcess inserts n filler events directly (AppendEvent does NOT rotate),
	// pushing the table over the cap without going through a rotating op.
	seedExcess := func(t *testing.T, s *Store, n int) {
		t.Helper()
		base := time.Now().Add(-time.Hour)
		for i := range n {
			if _, err := s.AppendEvent(ctx, domain.Event{
				Target:    domain.Target{Canonical: tcXGo},
				Kind:      EventLockAcquired,
				ActorUUID: tcAlice,
				Reason:    "filler",
				CreatedAt: base.Add(time.Duration(i) * time.Millisecond),
			}); err != nil {
				t.Fatalf("seed AppendEvent[%d]: %v", i, err)
			}
		}
	}

	assertTrimmed := func(t *testing.T, s *Store) {
		t.Helper()
		got, err := s.ListEvents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != eventsRetentionMax {
			t.Errorf("events not rotated by op under test: got %d, want %d", len(got), eventsRetentionMax)
		}
	}

	t.Run("release", func(t *testing.T) {
		s := mustOpen(t)
		l := mkFileLock(t, "a.go", tcAlice, time.Hour)
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		// Overfill AFTER acquire so the acquire's own rotation can't mask the gap.
		seedExcess(t, s, eventsRetentionMax+50)
		if _, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice); err != nil {
			t.Fatalf("release: %v", err)
		}
		assertTrimmed(t, s)
	})

	t.Run("release_by_session", func(t *testing.T) {
		s := mustOpen(t)
		l := mkFileLock(t, "a.go", tcAlice, time.Hour)
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		seedExcess(t, s, eventsRetentionMax+50)
		if _, err := s.ReleaseBySession(ctx, tcAlice, ""); err != nil {
			t.Fatalf("release-by-session: %v", err)
		}
		assertTrimmed(t, s)
	})

	t.Run("break", func(t *testing.T) {
		s := mustOpen(t)
		l := mkFileLock(t, "a.go", tcAlice, time.Hour)
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		seedExcess(t, s, eventsRetentionMax+50)
		// tcBob force-breaks tcAlice's lock.
		if _, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakForce, "x", "h", live); err != nil {
			t.Fatalf("break: %v", err)
		}
		assertTrimmed(t, s)
	})

	t.Run("downgrade", func(t *testing.T) {
		s := mustOpen(t)
		l := mkFileLock(t, "a.go", tcAlice, time.Hour) // acquired exclusive by default
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		seedExcess(t, s, eventsRetentionMax+50)
		if err := s.DowngradeLock(ctx, l.Target, tcAlice); err != nil {
			t.Fatalf("downgrade: %v", err)
		}
		assertTrimmed(t, s)
	})
}
