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

	owners, err := loadOwnersTx(ctx, tx, targets, byAgent)
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
		// Emit lock_released events in the same tx (atomic with the row deletes).
		now := time.Now()
		evs := make([]domain.Event, len(owned))
		for i, canonical := range owned {
			evs[i] = domain.Event{
				Target:    domain.Target{Canonical: canonical},
				Kind:      EventLockReleased,
				ActorUUID: byAgent,
				CreatedAt: now,
			}
		}
		if err := appendEventsTx(ctx, tx, evs); err != nil {
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
func classifyReleases(targets []domain.Target, owners map[string]ownerMode, byAgent string) ([]ReleaseResult, []string) {
	results := make([]ReleaseResult, len(targets))
	owned := make([]string, 0, len(targets))
	for i, t := range targets {
		results[i].Target = t
		o, ok := owners[t.Canonical]
		switch {
		case !ok:
			results[i].State = StateNoLock
		case o.Owner != byAgent:
			results[i].State = StateNotOwner
			results[i].Holder = o.Owner
		default:
			results[i].State = StateUnlocked
			results[i].Mode = o.Mode
			owned = append(owned, t.Canonical)
		}
	}
	return results, owned
}

func (s *Store) restoreAndAuditReleases(results []ReleaseResult, byAgent string) {
	now := time.Now()
	var failEvents []domain.Event
	var failIdx []int
	for i := range results {
		if results[i].State != StateUnlocked {
			continue
		}
		if (domain.LockRecord{Mode: results[i].Mode}).EffectiveMode() == domain.ModeShared {
			continue // shared lock never stripped the bit — nothing to restore
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].State = StateRestoreFailed
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
			failIdx = append(failIdx, i)
		}
	}
	if len(failEvents) > 0 {
		if auditErr := s.appendAuditDetached(failEvents); auditErr != nil {
			// Fan audit-write failure out to each affected result so callers
			// see the audit hole — silent loss was gh#107.
			for _, i := range failIdx {
				results[i].AuditErr = auditErr
			}
		}
	}
}

// ownerMode pairs a lock's owner with its mode for the release path's
// classify-then-restore decision (loto-k5el.2 T4).
type ownerMode struct{ Owner, Mode string }

// loadOwnersTx reads owner_uuid + mode for the given targets via a single
// SELECT. Returned map is keyed by target_canonical; missing keys = no row.
// Under the composite PK a shared target may have several holders; the map
// PREFERS byAgent's own row when present, so a holder can always release its
// own lock on a multi-holder shared target (otherwise an arbitrary other
// holder could shadow it and the release would misclassify as not-owner,
// leaving the caller's row undeleted — loto-k5el.2). When byAgent holds no row
// at a target, an arbitrary other holder is kept to drive the not-owner state.
func loadOwnersTx(ctx context.Context, tx *sql.Tx, targets []domain.Target, byAgent string) (map[string]ownerMode, error) {
	placeholders, args := inClause(targets)
	// placeholders is built from '?' chars only; user data flows via args.
	rows, err := tx.QueryContext(ctx, `SELECT target_canonical, owner_uuid, mode FROM locks WHERE target_canonical IN (`+placeholders+`)`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ownerMode, len(targets))
	for rows.Next() {
		var canonical, owner, mode string
		if err := rows.Scan(&canonical, &owner, &mode); err != nil {
			return nil, err
		}
		cur, seen := out[canonical]
		if !seen || (owner == byAgent && cur.Owner != byAgent) {
			out[canonical] = ownerMode{Owner: owner, Mode: mode}
		}
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

// ReleaseBySession atomically releases all locks owned by byAgent in the given
// session. If sessionUUID is empty, it releases all locks owned by byAgent
// regardless of session — the agent-scoped fallback for direct CLI use where
// no LOTO_SESSION_ID is pinned. This is the atomic replacement for the
// list+filter+release dance in unlockAll: a single SQL query finds matching
// rows and deletes them in one transaction, closing the TOCTOU gap where the
// old path could miss locks created between ListLocks and ReleaseLocks.
func (s *Store) ReleaseBySession(ctx context.Context, byAgent, sessionUUID string) ([]ReleaseResult, error) {
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

	// Find all targets matching agent (+session if pinned).
	canonicals, err := loadSessionTargetsTx(ctx, tx, byAgent, sessionUUID)
	if err != nil {
		return nil, err
	}
	if len(canonicals) == 0 {
		return []ReleaseResult{}, nil
	}
	paths := make([]string, len(canonicals))
	for i, c := range canonicals {
		paths[i] = c.Canonical
	}

	// Ack tags before deleting host locks (same ordering as ReleaseLocks).
	if err := ackTagsForReleaseTx(ctx, tx, paths, byAgent); err != nil {
		return nil, err
	}
	if err := deleteOwnedTx(ctx, tx, paths, byAgent); err != nil {
		return nil, err
	}
	// Emit lock_released events in the same tx (atomic with the row deletes).
	now := time.Now()
	evs := make([]domain.Event, len(canonicals))
	for i, c := range canonicals {
		evs[i] = domain.Event{
			Target:    domain.Target{Canonical: c.Canonical},
			Kind:      EventLockReleased,
			ActorUUID: byAgent,
			CreatedAt: now,
		}
	}
	if err := appendEventsTx(ctx, tx, evs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Build results and do chmod restore outside the tx.
	results := make([]ReleaseResult, len(canonicals))
	for i, c := range canonicals {
		results[i] = ReleaseResult{
			Target: domain.Target{Canonical: c.Canonical},
			State:  StateUnlocked,
			Mode:   c.Mode,
		}
	}
	s.restoreAndAuditReleases(results, byAgent)
	return results, nil
}

// sessionTarget pairs a session-owned lock's canonical path with its mode so
// the release restore guard can skip shared rows (loto-k5el.2 T4).
type sessionTarget struct {
	Canonical string
	Mode      string
}

// loadSessionTargetsTx returns canonical paths + modes for all locks owned by
// agent (and optionally scoped to session). Returns them in deterministic order.
func loadSessionTargetsTx(ctx context.Context, tx *sql.Tx, byAgent, sessionUUID string) ([]sessionTarget, error) {
	var rows *sql.Rows
	var err error
	if sessionUUID != "" {
		rows, err = tx.QueryContext(ctx,
			`SELECT target_canonical, mode FROM locks WHERE owner_uuid = ? AND session_uuid = ? ORDER BY target_canonical`,
			byAgent, sessionUUID)
	} else {
		rows, err = tx.QueryContext(ctx,
			`SELECT target_canonical, mode FROM locks WHERE owner_uuid = ? ORDER BY target_canonical`,
			byAgent)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionTarget
	for rows.Next() {
		var c sessionTarget
		if err := rows.Scan(&c.Canonical, &c.Mode); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
