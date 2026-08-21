package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// ReasonHolderChanged is the break path's rejection token, spelled to match
// the gate's RejectionReason vocabulary (`stale-lease-epoch`, `stale-preimage`
// — internal/gate/admission.go): kebab-case, names the fact, not the remedy.
// It is what `unlock --force` prints when the compare-and-swap fails.
const ReasonHolderChanged = "holder-changed"

// ErrHolderChanged is the sentinel behind every HolderChangedError, so callers
// match the class with errors.Is and recover the detail with errors.As.
var ErrHolderChanged = errors.New(ReasonHolderChanged)

// HolderChangedError reports a failed compare-and-swap: the caller stated the
// holds it believed it was breaking and the target's live hold set is not
// them. Carrying both sides is the point — "refused" without the two sets
// leaves the caller unable to tell a third agent's takeover from its own stale
// read, which is exactly the confusion loto-tqcw exists to end.
type HolderChangedError struct {
	Target   domain.Target
	Expected []domain.HoldRef // sorted
	Actual   []domain.HoldRef // sorted
}

func (e *HolderChangedError) Error() string {
	return fmt.Sprintf("%s: expected %s, found %s",
		ReasonHolderChanged, domain.FormatHoldRefs(e.Expected), domain.FormatHoldRefs(e.Actual))
}

// Is makes errors.Is(err, ErrHolderChanged) match, per the TargetValidationError
// precedent in locks.go: a struct error that still answers to a sentinel.
func (e *HolderChangedError) Is(target error) bool { return target == ErrHolderChanged }

// BreakExpectations is the compare-and-swap precondition for a break batch:
// canonical path → the exact hold set the caller believes it is breaking. When
// a target's live hold set differs, that target's break is refused with a
// HolderChangedError and nothing about it is written.
//
// ‡ A target ABSENT from the map carries NO precondition — the blind break
// --force meant before loto-tqcw. That mode stays reachable on purpose: a
// sweep over a dead peer's territory has no generations to name, and doctor's
// stale reclaim breaks rows it never read as a caller. What must not be
// silent is the CHOICE, so the CLI never defaults into it: `unlock --force`
// documents the bare form as the deliberate blind break, and the store's
// contract is that the map is the only door to CAS.
type BreakExpectations map[string][]domain.HoldRef

// BreakLocks force/stale-reclaims a batch of locks in one transaction. Per-target
// errors do not abort the batch — see BreakResult.Err. Returned error is non-nil
// only on internal/SQL failures. Results are returned in input order. Host
// policy rides inside `live` (HolderLiveProbe takes the record), so callers no
// longer pass a this-host string.
//
// expect is the per-target compare-and-swap precondition (nil = the whole
// batch is a blind break). It is checked BEFORE authorization and before any
// write: a target whose hold set moved under the caller is refused, keeps its
// rows, and emits no audit event — nothing was dispossessed, so there is
// nothing for invariant 8 to record.
func (s *Store) BreakLocks(ctx context.Context, targets []domain.Target, agent domain.AgentUUID, mode BreakMode, reason string, live domain.HolderLiveProbe, expect BreakExpectations) ([]BreakResult, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
	if len(targets) == 0 {
		return []BreakResult{}, nil
	}

	var results []BreakResult
	err := s.withLockBatchTx(ctx, targets, live, func(tx *sql.Tx, existing map[string][]domain.LockRecord, ec domain.EvalContext, now time.Time) error {
		var err error
		results, err = applyBreakChangesTx(ctx, tx, breakBatch{
			targets: targets, existing: existing, byAgent: byAgent,
			mode: mode, reason: reason, ec: ec, now: now, expect: expect,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// breakBatch is applyBreakChangesTx's input — one struct because the values
// travel together and a positional call of that width invites a transposed
// argument.
type breakBatch struct {
	targets  []domain.Target
	existing map[string][]domain.LockRecord
	byAgent  string
	mode     BreakMode
	reason   string
	ec       domain.EvalContext
	now      time.Time
	expect   BreakExpectations // nil, or per-target CAS preconditions
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
	results, events, deleteByOwner := classifyBreaks(b, force, kind)

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
// owner inside the same tx. force/kind are passed alongside b because
// applyBreakChangesTx already derived them from b.mode and re-deriving here
// would give the mode two readers.
func classifyBreaks(b breakBatch, force bool, kind string) (results []BreakResult, events []domain.Event, deleteByOwner map[string][]string) {
	results = make([]BreakResult, len(b.targets))
	deleteByOwner = map[string][]string{}
	for i, t := range b.targets {
		results[i].Target = t
		holders := b.existing[t.Canonical]
		// Compare-and-swap FIRST, ahead of the no-lock and authorization
		// branches (loto-tqcw). The caller named the holds it read; if the
		// target no longer carries exactly those, every later verdict is about
		// a hold the caller never saw — including "no lock at target", which
		// would report a vanished hold as a plain miss and hide that someone
		// released underneath. One rule, no branch where CAS quietly degrades
		// into a different message.
		if err := checkBreakExpectation(t, b.expect, holders); err != nil {
			results[i].Err = err
			continue
		}
		if len(holders) == 0 {
			results[i].Err = ErrNoLockAtTarget
			continue
		}
		// A target carries either one exclusive holder or N shared holders
		// (exclusive walls; shared coexist — NORTH_STAR I1/I2). Authorize the
		// whole set atomically: under stale-reclaim a single live co-holder
		// protects the target, so reject it entirely rather than break some
		// holders and leave others (loto-w77f).
		if err := authorizeHolders(holders, b.ec, force); err != nil {
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
				ActorUUID:   b.byAgent,
				SubjectUUID: owner,
				Reason:      b.reason,
				CreatedAt:   b.ec.Now,
			})
			deleteByOwner[owner] = append(deleteByOwner[owner], t.Canonical)
		}
	}
	return results, events, deleteByOwner
}

// checkBreakExpectation is the compare-and-swap gate. It returns nil when the
// caller stated no precondition for this target, and nil when the target's
// live hold set is exactly the one stated; otherwise a *HolderChangedError
// carrying both sides.
//
// ‡ The comparison is safe precisely because of where it runs. withLockBatchTx
// takes the project op-flock BEFORE reading the rows and holds it past the
// commit, so `holders` is a snapshot no peer can move under — the swap really
// is atomic with the compare. Doing this check in the caller, between a
// `status` read and a `--force` call, is the bug (loto-tqcw); doing it here is
// the fix.
func checkBreakExpectation(t domain.Target, expect BreakExpectations, holders []domain.LockRecord) error {
	want, stated := expect[t.Canonical]
	if !stated {
		return nil // blind break: the caller named no precondition
	}
	got := holdRefsOf(holders)
	sorted := make([]domain.HoldRef, len(want))
	copy(sorted, want) // never reorder the caller's slice
	domain.SortHoldRefs(sorted)
	if domain.HoldRefsEqual(sorted, got) {
		return nil
	}
	return &HolderChangedError{Target: t, Expected: sorted, Actual: got}
}

// holdRefsOf projects a target's live rows onto the (owner, epoch) identity
// the caller compares against, sorted. The composite PK makes owner unique per
// target, so the projection is injective — two rows can never collapse into
// one ref and hide a holder from the compare.
func holdRefsOf(holders []domain.LockRecord) []domain.HoldRef {
	refs := make([]domain.HoldRef, len(holders))
	for i := range holders {
		refs[i] = domain.HoldRef{Owner: holders[i].OwnerUUID, Epoch: holders[i].Epoch}
	}
	domain.SortHoldRefs(refs)
	return refs
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
