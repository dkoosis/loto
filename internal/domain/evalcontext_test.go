package domain

import (
	"testing"
	"time"
)

// EvalContext bundles the (now, live) pair that every staleness/authz predicate
// used to thread positionally. IsStale's behavior is exercised in
// staleness_test.go; here we pin the AuthorizeBreak gate over the same context.
func TestEvalContextAuthorizeBreak(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	ctx := EvalContext{Now: now, Live: aliveOn("h")}

	stale := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
	live := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}

	if err := ctx.AuthorizeBreak(stale, false); err != nil {
		t.Fatalf("stale lock breakable without force: %v", err)
	}
	if err := ctx.AuthorizeBreak(live, false); err == nil {
		t.Fatal("live lock must require force")
	}
	if err := ctx.AuthorizeBreak(live, true); err != nil {
		t.Fatalf("force must override: %v", err)
	}
}
