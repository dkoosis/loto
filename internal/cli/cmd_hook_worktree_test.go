package cli

import (
	"testing"
	"time"

	"loto/internal/domain"
)

// Two linked-worktree roots for the loto-3eq6 scoping tests in this package.
const (
	tcWtA = "/wt/a"
	tcWtB = "/wt/b"
)

// TestHookLiveHolders_SkipsSiblingWorktreeRows (loto-3eq6): the post-write
// observation reads files under this checkout, so a lock taken in a sibling
// worktree on the same repo-relative path must not mark this checkout's file
// Locked or lend it a holder and epoch. A legacy unstamped row still counts.
func TestHookLiveHolders_SkipsSiblingWorktreeRows(t *testing.T) {
	now := time.Now()
	ec := domain.EvalContext{Now: now, MyWorktree: tcWtA}
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: "a.go"}, OwnerUUID: "bob", ExpiresAt: now.Add(time.Hour), Worktree: tcWtB},
		{Target: domain.Target{Canonical: "b.go"}, OwnerUUID: "carol", ExpiresAt: now.Add(time.Hour), Worktree: tcWtA},
		{Target: domain.Target{Canonical: "c.go"}, OwnerUUID: "dave", ExpiresAt: now.Add(time.Hour)},
	}
	got := hookLiveHolders(locks, ec)
	if _, ok := got["a.go"]; ok {
		t.Errorf("a worktree-B row must not hold worktree A's a.go: %+v", got["a.go"])
	}
	if h, ok := got["b.go"]; !ok || h.OwnerUUID != "carol" {
		t.Errorf("a same-worktree row must hold b.go, got %+v (ok=%v)", h, ok)
	}
	if h, ok := got["c.go"]; !ok || h.OwnerUUID != "dave" {
		t.Errorf("a legacy unstamped row must still hold c.go, got %+v (ok=%v)", h, ok)
	}
}
