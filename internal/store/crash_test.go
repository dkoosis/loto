package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestCrash_FailedAddTagDoesNotAdvanceCursor — AddTag is its own transaction
// and does not touch read_cursors. We assert that a failed AddTag (e.g. via
// constraint violation) leaves no row, and read_cursors is unchanged.
func TestCrash_FailedAddTagDoesNotAdvanceCursor(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt, _ := domain.Canonicalize("a.go")

	// Seed a baseline cursor row.
	tg := domain.TagRecord{
		ID: "t-aaaa1111", Target: tgt, Kind: domain.TagNote,
		AuthorUUID: tcAlice, AddresseeUUID: tcBob, Intent: "first", CreatedAt: time.Now(),
	}
	if _, err := s.AddTag(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, tcBob, tgt); err != nil {
		t.Fatal(err)
	}
	var beforeCursor int64
	_ = s.db.QueryRow(`SELECT last_read_at FROM read_cursors WHERE agent_uuid='bob' AND target_canonical=?`, tgt.Canonical).Scan(&beforeCursor)

	// Now insert a tag with a colliding (target, id) → PK violation.
	bad := tg
	bad.Intent = "second"
	if _, err := s.AddTag(ctx, bad); err == nil {
		t.Fatal("expected PK violation on duplicate (target,id)")
	}
	var afterCursor int64
	_ = s.db.QueryRow(`SELECT last_read_at FROM read_cursors WHERE agent_uuid='bob' AND target_canonical=?`, tgt.Canonical).Scan(&afterCursor)
	if beforeCursor != afterCursor {
		t.Errorf("cursor advanced from %d to %d on failed AddTag", beforeCursor, afterCursor)
	}
}

// TestCrash_AcquireConflictNoPartialRow — a conflicting AcquireLocks returns
// *MultiConflictError without writing anything. The blocker remains, the
// loser's row is absent.
func TestCrash_AcquireConflictNoPartialRow(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	now := time.Now()

	dirLock := domain.LockRecord{
		Target:    domain.Target{Canonical: dir + "/", Kind: domain.KindDir},
		OwnerUUID: tcAlice, SessionUUID: tcAlice,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1,
	}
	fileLock := domain.LockRecord{
		Target:    domain.Target{Canonical: a, Kind: domain.KindFile},
		OwnerUUID: tcBob, SessionUUID: tcBob,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1,
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{dirLock}, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{fileLock}, live); err == nil {
		t.Fatal("expected conflict")
	}
	all, _ := s.ListLocks(ctx)
	for _, l := range all {
		if l.OwnerUUID == tcBob {
			t.Errorf("bob should have no lock row after conflict; got %+v", l)
		}
	}
}

// TestCrash_BreakLockAtomic — the system tag and the lock deletion both happen
// or neither does. We force a conflict (no force, live lock) and assert the
// row is unchanged and no system tag was written.
func TestCrash_BreakLockAtomic(t *testing.T) {
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	if err := s.BreakLock(ctx, l.Target, tcBob, false /*force*/, "x", live); err == nil {
		t.Fatal("expected break-without-force on live lock to fail")
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got == nil || got.OwnerUUID != tcAlice {
		t.Fatalf("lock should still belong to alice; got %+v", got)
	}
	tags, _ := s.TagsOnTarget(ctx, l.Target)
	if len(tags) != 0 {
		t.Errorf("no system tag should have been written; got %+v", tags)
	}
}
