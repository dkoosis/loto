package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

// Two linked-worktree roots for the loto-3eq6 scoping tests in this package.
const (
	tcWtA   = "/wt/a"
	tcWtB   = "/wt/b"
	tcCarol = "carol"
	tcDave  = "dave"
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
		{Target: domain.Target{Canonical: "b.go"}, OwnerUUID: tcCarol, ExpiresAt: now.Add(time.Hour), Worktree: tcWtA},
		{Target: domain.Target{Canonical: "c.go"}, OwnerUUID: tcDave, ExpiresAt: now.Add(time.Hour)},
	}
	got := hookLiveHolders(locks, ec, t.TempDir())
	if _, ok := got["a.go"]; ok {
		t.Errorf("a worktree-B row must not hold worktree A's a.go: %+v", got["a.go"])
	}
	if h, ok := got["b.go"]; !ok || h.OwnerUUID != tcCarol {
		t.Errorf("a same-worktree row must hold b.go, got %+v (ok=%v)", h, ok)
	}
	if h, ok := got["c.go"]; !ok || h.OwnerUUID != tcDave {
		t.Errorf("a legacy unstamped row must still hold c.go, got %+v (ok=%v)", h, ok)
	}
}

// TestHookLiveHolders_SkipsDirectoryHolds: a claim is a row in locks, but the
// hook observes files. A directory's stat moves with every file made beneath
// it, so observing a claim as a locked path would file false tree events.
func TestHookLiveHolders_SkipsDirectoryHolds(t *testing.T) {
	now := time.Now()
	top := t.TempDir()
	if err := os.MkdirAll(filepath.Join(top, "pkg", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	ec := domain.EvalContext{Now: now}
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: "."}, OwnerUUID: "bob", ExpiresAt: now.Add(time.Hour)},
		{Target: domain.Target{Canonical: "pkg/a"}, OwnerUUID: tcCarol, ExpiresAt: now.Add(time.Hour)},
		{Target: domain.Target{Canonical: "pkg/a/x.go"}, OwnerUUID: tcDave, ExpiresAt: now.Add(time.Hour)},
	}
	got := hookLiveHolders(locks, ec, top)
	if len(got) != 1 || got["pkg/a/x.go"].OwnerUUID != tcDave {
		t.Errorf("want only the file hold observed, got %+v", got)
	}
}
