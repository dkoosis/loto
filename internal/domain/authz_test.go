package domain

import (
	"testing"
	"time"
)

func TestAuthorizeUnlock(t *testing.T) {
	l := LockRecord{OwnerUUID: "alice"}
	if err := AuthorizeUnlock(l, "alice"); err != nil {
		t.Fatalf("owner unlock must succeed: %v", err)
	}
	if err := AuthorizeUnlock(l, "bob"); err == nil {
		t.Fatal("non-owner unlock must fail")
	}
}

func TestAuthorizeBreak(t *testing.T) {
	now := time.Now()
	stale := LockRecord{OwnerUUID: "alice", ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
	live := LockRecord{OwnerUUID: "alice", ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
	probe := func(string, int) bool { return true }

	if err := AuthorizeBreak(stale, "bob", false, now, "h", probe); err != nil {
		t.Fatalf("stale break without --force must succeed: %v", err)
	}
	if err := AuthorizeBreak(live, "bob", false, now, "h", probe); err == nil {
		t.Fatal("live break without --force must fail")
	}
	if err := AuthorizeBreak(live, "bob", true, now, "h", probe); err != nil {
		t.Fatalf("live break with --force must succeed: %v", err)
	}
}
