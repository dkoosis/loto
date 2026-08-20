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
// only on internal/SQL failures. Results are returned in input order. Host
// policy rides inside `live` (HolderLiveProbe takes the record), so callers no
// longer pass a this-host string.
func (s *Store) BreakLocks(ctx context.Context, targets []domain.Target, agent domain.AgentUUID, mode BreakMode, reason string, live domain.HolderLiveProbe) ([]BreakResult, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
	if len(targets) == 0 {
		return []BreakResult{}, nil
	}

	var results []BreakResult
	err := s.withLockBatchTx(ctx, targets, live, func(tx *sql.Tx, existing map[string][]domain.LockRecord, ec domain.EvalContext, now time.Time) error {
		var err error
		results, err = applyBreakChangesTx(ctx, tx, breakBatch{
			targets: targets, existing: existing, byAgent: byAgent,
			mode: mode, reason: reason, ec: ec, now: now,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// breakBatch is applyBreakChangesTx's input — one struct because the six
// values travel together and a positional call of that width invites a
// transposed argument.
type breakBatch struct {
	targets  []domain.Target
	existing map[string][]domain.LockRecord
	byAgent  string
	mode     BreakMode
	reason   string
	ec       domain.EvalContext
	now      time.Time
}

// applyBreakChangesTx classifies the batch and writes every consequence —
// audit events, the per-owner deletes, and the two retention sweeps — inside
// the caller's transaction.
func applyBreakChangesTx(ctx context.Context, tx *sql.Tx, b breakBatch) ([]BreakResult, error) {
	force := b.mode == BreakForce
	kind := EventLockBroken
	if !force {
		kind = EventLockReclaimedStale
	}
	results, events, deleteByOwner := classifyBreaks(b.targets, b.existing, b.byAgent, force, kind, b.reason, b.ec)

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
	if err := gcTagsTx(ctx, tx, b.now); err != nil {
		return nil, err
	}
	// Trim events in the same tx (mirrors AcquireLocks→rotateEventsTx). A
	// break-heavy workload (repeated `unlock --force` sweeps) that never
	// acquires would otherwise grow the events table unbounded (loto-bvdk).
	if err := rotateEventsTx(ctx, tx, b.now); err != nil {
		return nil, err
	}
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
			owner := string(holders[j].OwnerUUID) // map key + Event.SubjectUUID stay plain strings
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
