package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

// ReleaseLocks releases each target best-effort under the project op-flock in
// a single transaction (SELECT … WHERE IN, batched DELETE). Returns one
// ReleaseResult per input target in input order — render owns the canonical
// sort for stable output. The returned error is non-nil only on internal/SQL
// failures; per-target outcomes (no-lock, not-owner, restore-failed) are
// reported via ReleaseResult.State.
func (s *Store) ReleaseLocks(ctx context.Context, targets []domain.Target, byAgent string) ([]ReleaseResult, error) {
	if len(targets) == 0 {
		return []ReleaseResult{}, nil
	}

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	owners, err := loadOwnersTx(ctx, tx, targets)
	if err != nil {
		return nil, err
	}

	results, owned := classifyReleases(targets, owners, byAgent)

	if len(owned) > 0 {
		// Ack tags BEFORE deleting the host locks: the host-lock match must
		// still resolve to set acked_at; if we DELETE first the tags would
		// orphan instead, losing the audit ack (edge #6 distinguishes
		// release-ack from break-orphan).
		if err := ackTagsForReleaseTx(ctx, tx, owned, byAgent); err != nil {
			return nil, err
		}
		if err := deleteOwnedTx(ctx, tx, owned, byAgent); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Chmod restore is outside the tx — locks ARE released. Failures surface
	// per-target AND batch into one audit event call (NORTH_STAR.md: every path
	// that removes a `locks` row also tries restore + audits failure).
	s.restoreAndAuditReleases(results, byAgent)
	return results, nil
}

// classifyReleases walks input targets in order, classifying each against the
// owners map and collecting the canonical paths to delete in one statement.
func classifyReleases(targets []domain.Target, owners map[string]string, byAgent string) ([]ReleaseResult, []string) {
	results := make([]ReleaseResult, len(targets))
	owned := make([]string, 0, len(targets))
	for i, t := range targets {
		results[i].Target = t
		o, ok := owners[t.Canonical]
		switch {
		case !ok:
			results[i].State = StateNoLock
		case o != byAgent:
			results[i].State = StateNotOwner
			results[i].Holder = o
		default:
			results[i].State = StateUnlocked
			owned = append(owned, t.Canonical)
		}
	}
	return results, owned
}

func (s *Store) restoreAndAuditReleases(results []ReleaseResult, byAgent string) {
	now := time.Now()
	var failEvents []domain.Event
	for i := range results {
		if results[i].State != StateUnlocked {
			continue
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].State = StateRestoreFailed
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
		}
	}
	if len(failEvents) > 0 {
		_ = s.appendAuditDetached(failEvents)
	}
}

// loadOwnersTx reads owner_uuid for the given targets via a single SELECT.
// Returned map is keyed by target_canonical; missing keys = no row.
func loadOwnersTx(ctx context.Context, tx *sql.Tx, targets []domain.Target) (map[string]string, error) {
	placeholders, args := inClause(targets)
	// placeholders is built from '?' chars only; user data flows via args.
	rows, err := tx.QueryContext(ctx, `SELECT target_canonical, owner_uuid FROM locks WHERE target_canonical IN (`+placeholders+`)`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, len(targets))
	for rows.Next() {
		var canonical, owner string
		if err := rows.Scan(&canonical, &owner); err != nil {
			return nil, err
		}
		out[canonical] = owner
	}
	return out, rows.Err()
}

// ackTagsForReleaseTx marks every pending tag whose host lock is in the
// release set as acked. Run inside the release tx BEFORE deleteOwnedTx so the
// host-lock subquery still matches; running it after would silently orphan
// tags instead of acking them (would still get GC'd by doctor, but the audit
// would lose the explicit ack timestamp).
func ackTagsForReleaseTx(ctx context.Context, tx *sql.Tx, canonicals []string, byAgent string) error {
	placeholders, args := inClauseStrings(canonicals)
	args = append([]any{time.Now().UnixNano()}, args...)
	args = append(args, byAgent)
	_, err := tx.ExecContext(ctx, `UPDATE tags SET acked_at = ?`+ //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		` WHERE acked_at IS NULL`+
		` AND (target_canonical, lock_owner_uuid, lock_created_at) IN (`+
		`   SELECT target_canonical, owner_uuid, created_at FROM locks`+
		`   WHERE target_canonical IN (`+placeholders+`) AND owner_uuid = ?`+
		` )`, args...)
	return err
}

// deleteOwnedTx removes `locks` rows for the given canonical paths owned by
// byAgent in one statement.
func deleteOwnedTx(ctx context.Context, tx *sql.Tx, canonicals []string, byAgent string) error {
	placeholders, args := inClauseStrings(canonicals)
	args = append(args, byAgent)
	_, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical IN (`+placeholders+`) AND owner_uuid = ?`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	return err
}

// restoreAndAudit re-adds owner-write to a released target and emits a
// mode_restore_failed event on failure. Spec contract (NORTH_STAR.md): strip
// on acquire, restore on release. Callers: BreakLock, reclaimStaleTx,
// DoctorRepair — every path that removes a `locks` row. ReleaseLocks inlines
// the equivalent so it can also report per-target StateRestoreFailed.
func (s *Store) restoreAndAudit(ctx context.Context, path, byAgent string) {
	if err := restoreWrite(path); err != nil {
		_ = s.appendModeRestoreFailedEvent(ctx, path, byAgent, time.Now(), err)
	}
}
