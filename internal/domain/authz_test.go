package domain

import (
	"testing"
	"time"
)

func TestAuthorizeBreak(t *testing.T) {
	now := time.Now()
	stale := LockRecord{OwnerUUID: tcAlice, ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
	live := LockRecord{OwnerUUID: tcAlice, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
	probe := func(string, int, int64) bool { return true }

	if err := AuthorizeBreak(stale, false, now, "h", probe); err != nil {
		t.Fatalf("stale break without --force must succeed: %v", err)
	}
	if err := AuthorizeBreak(live, false, now, "h", probe); err == nil {
		t.Fatal("live break without --force must fail")
	}
	if err := AuthorizeBreak(live, true, now, "h", probe); err != nil {
		t.Fatalf("live break with --force must succeed: %v", err)
	}
}
