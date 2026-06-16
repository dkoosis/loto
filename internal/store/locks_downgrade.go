package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"loto/internal/domain"
)

// DowngradeLock flips an exclusive lock held by owner on target to shared, in
// place, and restores the owner-write bit — no unlock/relock, no new created_at
// (the hold is continuous). A lock that is already shared is a no-op resolved by
// a lock-free read probe — no immediate-mode write tx, no WAL writer lock
// (loto-kw5k). No lock at all returns ErrNoLockAtTarget. Emits a lock_downgraded audit event. The
// write-bit restore happens AFTER commit (mirrors release): the row state is
// authoritative; a restore failure is audited, not rolled back (loto-k5el.2).
func (s *Store) DowngradeLock(ctx context.Context, target domain.Target, owner string) error {
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	// Fast path (loto-kw5k): probe the mode with a plain read BEFORE opening
	// the immediate-mode write tx — beginTx takes SQLite's WAL writer lock at
	// BeginTx, which an already-shared no-op (or a missing lock) never needs.
	// The op-flock held above serializes lock mutators across processes, so
	// the probe is authoritative. Mirrors the migrate steady-state fast path
	// (store.go).
	var curMode string
	row := s.db.QueryRowContext(ctx,
		`SELECT mode FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		target.Canonical, owner)
	if err := row.Scan(&curMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoLockAtTarget
		}
		return err
	}
	if curMode == domain.ModeShared {
		// Already shared — no mode write needed, so no write tx (loto-kw5k).
		// But the write bit can be stale-stripped relative to the row: an
		// earlier downgrade whose post-commit restoreWrite failed (audited
		// mode_restore_failed, row already shared) or a crash between commit
		// and restore leaves the file read-only with no further remedy, since
		// re-running downgrade lands here. restoreWrite is idempotent (only
		// adds owner-write, missing-file is a no-op), so reconcile it now —
		// matching the post-commit restore semantics of the excl→shared branch
		// below. Audited-not-rolled-back on failure (loto-k5el.2). This is a
		// pure filesystem op, so it preserves the no-write-tx property.
		now := time.Now()
		if rerr := restoreWrite(target.Canonical); rerr != nil {
			_ = s.appendAuditDetached([]domain.Event{
				modeRestoreFailedEvent(target.Canonical, owner, now, rerr),
			})
			return &ChmodFailureError{Failures: []ChmodFailure{
				{Target: target, Err: rerr, RolledBack: false},
			}}
		}
		return nil
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET mode = ? WHERE target_canonical = ? AND owner_uuid = ?`,
		domain.ModeShared, target.Canonical, owner); err != nil {
		return err
	}
	if err := appendEventTx(ctx, tx, domain.Event{
		Target:    target,
		Kind:      EventLockDowngraded,
		ActorUUID: owner,
		Reason:    "exclusive→shared",
		CreatedAt: now,
	}); err != nil {
		return err
	}
	// Trim events in the same tx (mirrors AcquireLocks→rotateEventsTx). A
	// downgrade-heavy workload (read-mode churn) that rarely acquires would
	// otherwise grow the events table unbounded (loto-bvdk).
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Restore the write bit outside the tx — the row is authoritative now.
	if rerr := restoreWrite(target.Canonical); rerr != nil {
		_ = s.appendAuditDetached([]domain.Event{
			modeRestoreFailedEvent(target.Canonical, owner, now, rerr),
		})
		return &ChmodFailureError{Failures: []ChmodFailure{
			{Target: target, Err: rerr, RolledBack: false},
		}}
	}
	return nil
}
