package domain

import (
	"testing"
	"time"
)

func TestAuthorizeBreak(t *testing.T) {
	now := time.Now()
	stale := LockRecord{OwnerUUID: tcAlice, ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
	live := LockRecord{OwnerUUID: tcAlice, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
	ctx := EvalContext{Now: now, Live: aliveOn("h")}

	if err := ctx.AuthorizeBreak(stale, false); err != nil {
		t.Fatalf("stale break without --force must succeed: %v", err)
	}
	if err := ctx.AuthorizeBreak(live, false); err == nil {
		t.Fatal("live break without --force must fail")
	}
	if err := ctx.AuthorizeBreak(live, true); err != nil {
		t.Fatalf("live break with --force must succeed: %v", err)
	}
}
