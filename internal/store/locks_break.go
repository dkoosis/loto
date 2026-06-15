package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

// BreakMode selects between operator-initiated break and stale-only reclaim.
// Replaces the prior `force bool` parameter (domain-vocab bool-trap finding,
// review run a608d43c6832 theme 3): call sites used to read
// `BreakLocks(..., true /*force*/, ...)` with comment-as-documentation.
type BreakMode int

const (
	// BreakForce: operator-initiated. Authorizes live locks; emits lock_broken.
	BreakForce BreakMode = iota
	// BreakStale: stale-only reclaim. Refuses live locks; emits lock_reclaimed_stale.
	BreakStale
)

// BreakLocks force/stale-reclaims a batch of locks in one transaction. Per-target
// errors do not abort the batch — see BreakResult.Err. Returned error is non-nil
// only on internal/SQL failures. Results are returned in input order.
func (s *Store) BreakLocks(ctx context.Context, targets []domain.Target, byAgent string, mode BreakMode, reason string, thisHost string, live domain.PidLiveProbe) ([]BreakResult, error) {
	if len(targets) == 0 {
		return []BreakResult{}, nil
	}

	// Hold the op-flock across the tx AND the post-commit restoreWrite so
	// concurrent AcquireLocks can't observe a row+file pair where one side
	// of the chmod has lagged (gh#... loto-4qt).
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

	existing, err := loadLocksByTargetTx(ctx, tx, targets)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	ec := domain.EvalContext{Now: now, ThisHost: thisHost, Live: live}
	force := mode == BreakForce
	kind := EventLockBroken
	if !force {
		kind = EventLockReclaimedStale
	}

	results, events, deleteByOwner := classifyBreaks(targets, existing, byAgent, force, kind, reason, ec)

	if len(events) > 0 {
		if err := appendEventsTx(ctx, tx, events); err != nil {
			return nil, err
		}
	}
	for owner, canonicals := range deleteByOwner {
		if err := deleteOwnedTx(ctx, tx, canonicals, owner); err != nil {
			return nil, err
		}
	}
	// Reclaim the tags orphaned by the deletes above in the SAME tx. Break
	// removes host-lock rows without acking their tags (a broken peer never
	// "reads" its notes), so without this the orphans would linger until an
	// operator ran `doctor --repair` — unbounded retention on a path the hot
	// loop never triggers (loto-qg0r). gcTagsTx is the same disk-reclamation
	// pass doctor uses; running it here mirrors AcquireLocks→rotateEventsTx.
	if err := gcTagsTx(ctx, tx, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.restoreAndAuditBreaks(results, byAgent, now)
	return results, nil
}

// classifyBreaks walks input targets in order, building per-target results, the
// batched event slice, and a per-owner canonical-path grouping for DELETE.
// Returning all three lets the caller emit one events insert and one DELETE per
// owner inside the same tx.
func classifyBreaks(
	targets []domain.Target,
	existing map[string][]domain.LockRecord,
	byAgent string,
	force bool,
	kind string,
	reason string,
	ec domain.EvalContext,
) (results []BreakResult, events []domain.Event, deleteByOwner map[string][]string) {
	results = make([]BreakResult, len(targets))
	deleteByOwner = map[string][]string{}
	for i, t := range targets {
		results[i].Target = t
		holders := existing[t.Canonical]
		if len(holders) == 0 {
			results[i].Err = ErrNoLockAtTarget
			continue
		}
		// A target carries either one exclusive holder or N shared holders
		// (exclusive walls; shared coexist — NORTH_STAR I1/I2). Authorize the
		// whole set atomically: under stale-reclaim a single live co-holder
		// protects the target, so reject it entirely rather than break some
		// holders and leave others (loto-w77f).
		if err := authorizeHolders(holders, ec, force); err != nil {
			results[i].Err = err
			continue
		}
		// All holders share one mode (the all-shared-or-one-exclusive
		// invariant), so holders[0].Mode drives the restore decision.
		results[i].Mode = holders[0].Mode
		for j := range holders {
			owner := holders[j].OwnerUUID
			events = append(events, domain.Event{
				Target:      t,
				Kind:        kind,
				ActorUUID:   byAgent,
				SubjectUUID: owner,
				Reason:      reason,
				CreatedAt:   ec.Now,
			})
			deleteByOwner[owner] = append(deleteByOwner[owner], t.Canonical)
		}
	}
	return results, events, deleteByOwner
}

// authorizeHolders returns the first AuthorizeBreak failure across a target's
// holders, or nil if every holder may be broken. Under BreakForce all holders
// pass; under stale-reclaim one live holder vetoes the whole target.
func authorizeHolders(holders []domain.LockRecord, ec domain.EvalContext, force bool) error {
	for i := range holders {
		if err := ec.AuthorizeBreak(holders[i], force); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) restoreAndAuditBreaks(results []BreakResult, byAgent string, now time.Time) {
	var failEvents []domain.Event
	var failIdx []int
	for i := range results {
		if results[i].Err != nil {
			continue
		}
		if !shouldRestoreOwnerWrite(results[i].Mode) {
			continue // shared lock never stripped the bit — nothing to restore
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
			failIdx = append(failIdx, i)
		}
	}
	if len(failEvents) > 0 {
		if auditErr := s.appendAuditDetached(failEvents); auditErr != nil {
			// Fan audit-write failure out to each affected result (gh#107).
			for _, i := range failIdx {
				results[i].AuditErr = auditErr
			}
		}
	}
}

// loadLocksByTargetTx groups every holder per target. A target under the
// composite PK (target_canonical, owner_uuid) may carry several coexisting
// shared holders; keying the result by canonical ALONE collapsed them to the
// last-scanned row, so a multi-holder break removed one arbitrary holder and
// reported success while the others silently survived (loto-w77f). ORDER BY
// makes the per-holder event/delete stream deterministic.
func loadLocksByTargetTx(ctx context.Context, tx *sql.Tx, targets []domain.Target) (map[string][]domain.LockRecord, error) {
	placeholders, args := inClause(targets)
	rows, err := tx.QueryContext(ctx, `SELECT `+lockCols+` FROM locks WHERE target_canonical IN (`+placeholders+`) ORDER BY created_at ASC, owner_uuid ASC`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]domain.LockRecord, len(targets))
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out[l.Target.Canonical] = append(out[l.Target.Canonical], l)
	}
	return out, rows.Err()
}
