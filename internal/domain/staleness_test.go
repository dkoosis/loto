package domain

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	live := func(string, int) bool { return true }
	dead := func(string, int) bool { return false }

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
}
