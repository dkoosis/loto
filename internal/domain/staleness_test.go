package domain

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	live := func(string, int, int64) bool { return true }
	dead := func(string, int, int64) bool { return false }

	t.Run("past TTL is stale", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
		if !IsStale(l, now, "h", live) {
			t.Fatal("past TTL must be stale")
		}
	})
	t.Run("dead pid same host is stale even when TTL not reached", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if !IsStale(l, now, "h", dead) {
			t.Fatal("dead pid on same host must be stale")
		}
	})
	t.Run("dead pid other host is NOT stale (out of scope)", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "other", PID: 1}
		if IsStale(l, now, "this", dead) {
			t.Fatal("dead pid on other host must not stale-flag")
		}
	})
	t.Run("live and within TTL is not stale", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if IsStale(l, now, "h", live) {
			t.Fatal("live within TTL must not be stale")
		}
	})

	// PID-reuse recycle protection (loto-kwlp). The probe receives the lock's
	// stored start-time and may report a pid dead when the current occupant's
	// start-time differs — even though Kill(pid,0) alone would say "alive".
	recycleAware := func(_ string, _ int, storedStart int64) bool {
		const currentStart = 5000
		if storedStart != 0 && storedStart != currentStart {
			return false // recycled: pid alive but not our original holder
		}
		return true
	}

	t.Run("recycled pid (stored start differs from occupant) is stale", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 4000}
		if !IsStale(l, now, "h", recycleAware) {
			t.Fatal("recycled pid (start mismatch) must be stale")
		}
	})
	t.Run("matching start-time is not stale", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 5000}
		if IsStale(l, now, "h", recycleAware) {
			t.Fatal("matching start-time must not be stale")
		}
	})
	t.Run("legacy row (ProcStart 0) falls back to pid-alive-only", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 0}
		if IsStale(l, now, "h", recycleAware) {
			t.Fatal("unknown start-time (legacy) must fall back to pid-alive: not stale when alive")
		}
	})
}
