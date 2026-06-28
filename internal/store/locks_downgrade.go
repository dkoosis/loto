package store

import (
	"context"
	"time"

	"loto/internal/domain"
)

// DowngradeResult is the per-target outcome from DowngradeLocks, returned in
// input order. Err is ErrNoLockAtTarget when owner holds no lock at the target,
// nil otherwise. RestoreErr is set independently when the row reached shared but
// the post-commit owner-write restore failed — the downgrade succeeded but the
// file is left read-only (audited via mode_restore_failed, not rolled back —
// loto-k5el.2), mirroring BreakResult.RestoreErr. AuditErr is set when that
// audit event could not be persisted (gh#107).
type DowngradeResult struct {
	Target     domain.Target
	Err        error
	RestoreErr error
	AuditErr   error
}

// DowngradeLocks flips each exclusive lock held by owner to shared, in place,
// and restores the owner-write bit — no unlock/relock, no new created_at (the
// hold is continuous). Per-target errors do not abort the batch (see
// DowngradeResult.Err); the returned error is non-nil only on internal/SQL
// failures. Mirrors BreakLocks/ReleaseLocks: one op-flock, one tx, post-commit
// chmod restore under the still-held flock (loto-4qt), detached audit off the
// critical section (loto-3qev). Callers pass distinct targets — the CLI dedups
// via validateLockTargets (cmd_lock.go) — so, like the sibling batch methods,
// this does not guard against duplicate input.
//
// Fast path (loto-kw5k): every target's mode is probed with a plain read BEFORE
// any write tx. When no target needs a mode flip (all already shared, or no lock
// held), beginTx — which takes SQLite's WAL writer lock at BeginTx — is never
// opened. Already-shared targets still get an idempotent restoreWrite reconcile
// (loto-1jxc): a prior downgrade whose post-commit restore failed, or a crash
// between commit and restore, leaves the file read-only on a shared row;
// re-running downgrade heals it without a write tx.
func (s *Store) DowngradeLocks(ctx context.Context, targets []domain.Target, ownerID domain.AgentUUID) ([]DowngradeResult, error) {
	owner := string(ownerID) // internal store helpers thread the owner as a plain string
	if len(targets) == 0 {
		return []DowngradeResult{}, nil
	}

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	modeByCanon, err := s.probeOwnerModes(ctx, owner, targets)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	results := make([]DowngradeResult, len(targets))
	var flip []domain.Target // exclusive targets needing UPDATE → shared
	var events []domain.Event
	for i, t := range targets {
		results[i].Target = t
		mode, held := modeByCanon[t.Canonical]
		switch {
		case !held:
			results[i].Err = ErrNoLockAtTarget
		case mode == domain.ModeShared:
			// Already shared — no mode write (loto-kw5k); reconciled below.
		default:
			flip = append(flip, t)
			events = append(events, domain.Event{
				Target:    t,
				Kind:      EventLockDowngraded,
				ActorUUID: owner,
				Reason:    "exclusive→shared",
				CreatedAt: now,
			})
		}
	}

	// Open the immediate-mode write tx ONLY when a flip is needed (loto-kw5k).
	if len(flip) > 0 {
		if err := s.commitDowngrades(ctx, flip, owner, events, now); err != nil {
			return nil, err
		}
	}

	// Restore the owner-write bit under the still-held flock (loto-4qt), release
	// it, THEN emit the detached audit off the critical section (loto-3qev). The
	// deferred release above is the idempotent backstop.
	failEvents, failIdx := restoreDowngrades(results, owner, now)
	flock.release()
	if len(failEvents) > 0 {
		if auditErr := s.appendAuditDetached(failEvents); auditErr != nil {
			for _, i := range failIdx {
				results[i].AuditErr = auditErr
			}
		}
	}
	return results, nil
}

// probeOwnerModes reads the current mode of each target held by owner in one
// plain SELECT — no write tx (loto-kw5k). The op-flock held by the caller
// serializes lock mutators across processes, so the read is authoritative.
// Targets owner holds no lock on are absent from the returned map.
func (s *Store) probeOwnerModes(ctx context.Context, owner string, targets []domain.Target) (map[string]string, error) {
	ph, canonArgs := inClause(targets)
	args := append([]any{owner}, canonArgs...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT target_canonical, mode FROM locks WHERE owner_uuid = ? AND target_canonical IN (`+ph+`)`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	modeByCanon := make(map[string]string, len(targets))
	for rows.Next() {
		var canon, mode string
		if err := rows.Scan(&canon, &mode); err != nil {
			return nil, err
		}
		modeByCanon[canon] = mode
	}
	return modeByCanon, rows.Err()
}

// commitDowngrades flips the given exclusive targets to shared and appends the
// lock_downgraded events in one immediate-mode write tx, trimming the events
// table in the same tx (mirrors AcquireLocks→rotateEventsTx; a downgrade-heavy
// read-mode churn that rarely acquires would otherwise grow events unbounded —
// loto-bvdk).
func (s *Store) commitDowngrades(ctx context.Context, flip []domain.Target, owner string, events []domain.Event, now time.Time) error {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	ph, canonArgs := inClause(flip)
	args := append([]any{domain.ModeShared, owner}, canonArgs...)
	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET mode = ? WHERE owner_uuid = ? AND target_canonical IN (`+ph+`)`, args...); err != nil { //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		return err
	}
	if err := appendEventsTx(ctx, tx, events); err != nil {
		return err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

// restoreDowngrades restores the owner-write bit for every target that now holds
// shared (Err == nil — both just-flipped and already-shared reconcile), records
// per-result RestoreErr, and returns the mode_restore_failed events plus the
// parallel result indices for the caller's detached audit. Chmod-only half: the
// CALLER runs it under the held op-flock (loto-4qt) and audits after releasing
// the flock (loto-3qev). Mirrors restoreBreaks.
func restoreDowngrades(results []DowngradeResult, owner string, now time.Time) ([]domain.Event, []int) {
	var failEvents []domain.Event
	var failIdx []int
	for i := range results {
		if results[i].Err != nil {
			continue
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, owner, now, rerr))
			failIdx = append(failIdx, i)
		}
	}
	return failEvents, failIdx
}

// downgradeLock flips the single exclusive lock held by owner on target to
// shared. Thin n=1 wrapper over DowngradeLocks preserving the original
// error-returning contract: ErrNoLockAtTarget when no lock is held, a
// *ChmodFailureError when the row reached shared but the post-commit write-bit
// restore failed (loto-k5el.2).
func (s *Store) downgradeLock(ctx context.Context, target domain.Target, owner domain.AgentUUID) error { //nolint:unparam // owner scopes the downgrade contract; test-only wrapper currently exercised with a single owner
	results, err := s.DowngradeLocks(ctx, []domain.Target{target}, owner)
	if err != nil {
		return err
	}
	r := results[0]
	if r.Err != nil {
		return r.Err
	}
	if r.RestoreErr != nil {
		return &ChmodFailureError{Failures: []ChmodFailure{
			{Target: target, Err: r.RestoreErr, RolledBack: false},
		}}
	}
	return nil
}
