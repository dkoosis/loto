package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestDowngrade_ExclusiveToShared_RestoresWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected stripped before downgrade")
	}
	if err := s.DowngradeLock(ctx, rec.Target, tcAlice); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	l, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if l == nil || l.EffectiveMode() != domain.ModeShared {
		t.Fatalf("want shared after downgrade, got %v", l)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("downgrade must restore owner-write; perm=%v", fi.Mode().Perm())
	}
}

func TestDowngrade_NoLock_Errors(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour) // file exists, no lock
	err := s.DowngradeLock(ctx, rec.Target, tcAlice)
	if !errors.Is(err, ErrNoLockAtTarget) {
		t.Fatalf("want ErrNoLockAtTarget, got %v", err)
	}
}

func TestDowngrade_AlreadyShared_NoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.DowngradeLock(ctx, rec.Target, tcAlice); err != nil {
		t.Fatalf("downgrade of already-shared should be a no-op, got %v", err)
	}
}

// TestDowngrade_AlreadyShared_ReconcilesStaleStrippedWriteBit asserts the
// already-shared fast path restores a write bit that was stale-stripped
// relative to the row (loto-1jxc): a prior downgrade's post-commit
// restoreWrite failed, or a crash landed between commit and restore, leaving
// the file read-only on a shared row. Re-running downgrade lands on the
// fast path, which must reconcile the bit idempotently rather than no-op.
func TestDowngrade_AlreadyShared_ReconcilesStaleStrippedWriteBit(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate the stale-stripped bit: row is shared, file is read-only.
	if err := os.Chmod(rec.Target.Canonical, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := s.DowngradeLock(ctx, rec.Target, tcAlice); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	l, _ := s.LockForOwnerAt(ctx, rec.Target, tcAlice)
	if l == nil || l.EffectiveMode() != domain.ModeShared {
		t.Fatalf("want shared still, got %v", l)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("already-shared downgrade must reconcile owner-write; perm=%v", fi.Mode().Perm())
	}
}

// TestDowngrade_AlreadyShared_ReconcileUsesNoWriteTx asserts the reconcile on
// the already-shared fast path stays a pure filesystem op (loto-1jxc) — it
// must not open the immediate-mode write tx (loto-kw5k). A peer holds the WAL
// writer lock; the restore-and-return must still succeed without stalling on
// it. File is left read-only so restoreWrite has real work to do.
func TestDowngrade_AlreadyShared_ReconcileUsesNoWriteTx(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := os.Chmod(rec.Target.Canonical, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	db2, err := sql.Open("sqlite", connDSN(s.dbPath))
	if err != nil {
		t.Fatalf("open peer db: %v", err)
	}
	defer db2.Close()
	tx2, err := db2.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("peer BeginTx: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()

	dlCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.DowngradeLock(dlCtx, rec.Target, tcAlice); err != nil {
		t.Fatalf("reconcile on already-shared must not open a write tx, got %v", err)
	}
	if fi, _ := os.Stat(rec.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("expected owner-write reconciled; perm=%v", fi.Mode().Perm())
	}
}

// TestDowngrade_AlreadyShared_NoWriteTx asserts the already-shared no-op
// avoids the immediate-mode write tx (loto-kw5k): the mode is probed with a
// plain read before beginTx, so the no-op must succeed even while a peer
// connection holds SQLite's WAL writer lock. Without the fix, beginTx's
// BEGIN IMMEDIATE blocks on the held writer lock until the ctx-scaled
// busy_timeout and fails SQLITE_BUSY.
func TestDowngrade_AlreadyShared_NoWriteTx(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	rec := mkFileLock(t, "a.go", tcAlice, time.Hour)
	rec.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Hold the WAL writer lock from a peer connection. connDSN sets
	// _txlock=immediate, so BeginTx issues BEGIN IMMEDIATE and takes the
	// writer lock right here.
	db2, err := sql.Open("sqlite", connDSN(s.dbPath))
	if err != nil {
		t.Fatalf("open peer db: %v", err)
	}
	defer db2.Close()
	tx2, err := db2.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("peer BeginTx: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()

	// Bound the call: a write tx would stall on the peer's writer lock for
	// the full ctx-scaled busy_timeout, then surface SQLITE_BUSY.
	dlCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.DowngradeLock(dlCtx, rec.Target, tcAlice); err != nil {
		t.Fatalf("already-shared downgrade must not open a write tx, got %v", err)
	}
}

// TestDowngradeLocks_Batch_MixedTargets exercises the batched path (loto-r2wc):
// one exclusive, one already-shared (with a stale-stripped bit), and one
// no-lock target in a single call. Results come back in input order; the
// exclusive flips and restores, the shared reconciles its bit, the no-lock
// target reports ErrNoLockAtTarget — none aborting the others.
func TestDowngradeLocks_Batch_MixedTargets(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	excl := mkFileLock(t, "excl.go", tcAlice, time.Hour)
	excl.Mode = domain.ModeExclusive
	shared := mkFileLock(t, "shared.go", tcAlice, time.Hour)
	shared.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{excl, shared}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	nolock := mkFileLock(t, "nolock.go", tcAlice, time.Hour) // file exists, no lock held

	// Stale-stripped bit on the already-shared target (loto-1jxc): row shared,
	// file read-only — the reconcile must heal it.
	if err := os.Chmod(shared.Target.Canonical, 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	targets := []domain.Target{excl.Target, shared.Target, nolock.Target}
	results, err := s.DowngradeLocks(ctx, targets, tcAlice)
	if err != nil {
		t.Fatalf("DowngradeLocks: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	for i, want := range targets {
		if results[i].Target.Canonical != want.Canonical {
			t.Errorf("result[%d] target=%s want %s", i, results[i].Target.Canonical, want.Canonical)
		}
	}

	// [0] exclusive → shared, write bit restored.
	if results[0].Err != nil || results[0].RestoreErr != nil {
		t.Errorf("excl: unexpected err=%v restoreErr=%v", results[0].Err, results[0].RestoreErr)
	}
	if l, _ := s.LockForOwnerAt(ctx, excl.Target, tcAlice); l == nil || l.EffectiveMode() != domain.ModeShared {
		t.Errorf("excl not shared after downgrade: %v", l)
	}
	if fi, _ := os.Stat(excl.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("excl write bit not restored; perm=%v", fi.Mode().Perm())
	}

	// [1] already-shared → stays shared, stale-stripped bit reconciled.
	if results[1].Err != nil || results[1].RestoreErr != nil {
		t.Errorf("shared: unexpected err=%v restoreErr=%v", results[1].Err, results[1].RestoreErr)
	}
	if fi, _ := os.Stat(shared.Target.Canonical); fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("shared write bit not reconciled; perm=%v", fi.Mode().Perm())
	}

	// [2] no lock held → ErrNoLockAtTarget.
	if !errors.Is(results[2].Err, ErrNoLockAtTarget) {
		t.Errorf("nolock: want ErrNoLockAtTarget, got %v", results[2].Err)
	}
}
