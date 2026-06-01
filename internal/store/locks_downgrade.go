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
// (the hold is continuous). A lock that is already shared is a no-op. No lock at
// all returns ErrNoLockAtTarget. Emits a lock_downgraded audit event. The
// write-bit restore happens AFTER commit (mirrors release): the row state is
// authoritative; a restore failure is audited, not rolled back (loto-k5el.2).
func (s *Store) DowngradeLock(ctx context.Context, target domain.Target, owner string) error {
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var curMode string
	row := tx.QueryRowContext(ctx,
		`SELECT mode FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		target.Canonical, owner)
	if err := row.Scan(&curMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoLockAtTarget
		}
		return err
	}
	if (domain.LockRecord{Mode: curMode}).EffectiveMode() == domain.ModeShared {
		return tx.Commit() // already shared — no-op
	}

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
