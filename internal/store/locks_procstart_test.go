package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestProcStartRoundTrip confirms proc_start survives acquire → read-back, and
// that the unknown (0) value round-trips through the nullable column as 0
// (loto-kwlp). It also asserts the IsStale fallback: a legacy/unknown row
// behaves as pid-alive-only, while a known start-time that mismatches the
// current occupant is treated as stale.
func TestProcStartRoundTrip(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	t.Run("known proc_start persists", func(t *testing.T) {
		l := mkFileLock(t, "known.go", tcAlice, time.Hour)
		l.ProcStart = 123456789
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatal(err)
		}
		got, err := s.LockAt(ctx, l.Target)
		if err != nil || got == nil {
			t.Fatalf("LockAt: %v / %v", got, err)
		}
		if got.ProcStart != 123456789 {
			t.Fatalf("ProcStart = %d, want 123456789", got.ProcStart)
		}
	})

	t.Run("unknown proc_start round-trips as 0 (NULL)", func(t *testing.T) {
		l := mkFileLock(t, "legacy.go", tcBob, time.Hour)
		l.ProcStart = 0 // unknown / legacy
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
			t.Fatal(err)
		}
		got, err := s.LockAt(ctx, l.Target)
		if err != nil || got == nil {
			t.Fatalf("LockAt: %v / %v", got, err)
		}
		if got.ProcStart != 0 {
			t.Fatalf("ProcStart = %d, want 0 (unknown)", got.ProcStart)
		}
	})

	t.Run("IsStale: known mismatch stale, unknown falls back", func(t *testing.T) {
		now := time.Now()
		recycleAware := func(_ string, _ int, storedStart int64) bool {
			const occupant = 999
			if storedStart != 0 && storedStart != occupant {
				return false
			}
			return true
		}
		known := domain.LockRecord{Host: "h", PID: 1, ProcStart: 123456789, ExpiresAt: now.Add(time.Hour)}
		if !domain.IsStale(known, now, "h", recycleAware) {
			t.Fatal("known proc_start mismatching occupant must be stale")
		}
		legacy := domain.LockRecord{Host: "h", PID: 1, ProcStart: 0, ExpiresAt: now.Add(time.Hour)}
		if domain.IsStale(legacy, now, "h", recycleAware) {
			t.Fatal("unknown proc_start must fall back to pid-alive (not stale)")
		}
	})
}
