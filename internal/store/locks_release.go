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
// failures; per-target outcomes (no-lock, not-owner, reclaimed-stale,
// restore-failed) are reported via ReleaseResult.State.
//
// Stale-aware (loto-ebkc): when the caller holds no row at a target and EVERY
// foreign holder is stale under (thisHost, live), the plain unlock reclaims
// them all — delete + lock_reclaimed_stale audit in the same tx — instead of
// bouncing to not-owner. One live foreign holder vetoes the whole target
// (authorizeHolders, loto-w77f parity). The caller's own row always wins
// first (prefer-own, loto-k5el.2) and never piggybacks a co-holder reclaim.
func (s *Store) ReleaseLocks(ctx context.Context, targets []domain.Target, agent domain.AgentUUID, thisHost string, live domain.PidLiveProbe) ([]ReleaseResult, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
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

	existing, err := loadLocksByTargetTx(ctx, tx, targets)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	ec := domain.EvalContext{Now: now, ThisHost: thisHost, Live: live}
	results, owned, reclaims := classifyReleases(targets, existing, byAgent, ec)

	if err := s.applyReleaseChangesTx(ctx, tx, owned, reclaims, byAgent, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Chmod restore is outside the tx — locks ARE released. Failures surface
	// per-target AND batch into one audit event call (NORTH_STAR.md: every path
	// that removes a `locks` row also tries restore + audits failure). The
	// bounded chmod runs under the still-held flock; we release BEFORE the
	// detached audit so its write tx can't extend the flock hold (loto-3qev).
	s.restoreReleasesThenAudit(flock, results, byAgent)
	return results, nil
}

// applyReleaseChangesTx writes both release branches into the caller's tx:
// the batched own-row releases (ack + delete + lock_released events) and the
// per-holder stale reclaims (lock_reclaimed_stale + delete via reclaimStaleTx).
// Reclaims orphan the dead holders' pending tags (a dead peer never "reads"
// its notes) — those are GC'd per holder via gcTagsForReclaimTx (loto-qg0r
// intent, targeted form). Event rotation fires when EITHER branch appended
// (mirrors AcquireLocks→rotateEventsTx): a release/reclaim-heavy workload that
// rarely acquires would otherwise grow the events table unbounded (loto-bvdk).
func (s *Store) applyReleaseChangesTx(ctx context.Context, tx *sql.Tx, owned []string, reclaims []domain.LockRecord, byAgent string, now time.Time) error {
	if len(owned) > 0 {
		if err := s.applyOwnedReleasesTx(ctx, tx, owned, byAgent, now); err != nil {
			return err
		}
	}
	for i := range reclaims {
		if err := reclaimStaleTx(ctx, tx, reclaims[i], byAgent, now); err != nil {
			return err
		}
		if err := gcTagsForReclaimTx(ctx, tx, reclaims[i]); err != nil {
			return err
		}
	}
	if len(owned) > 0 || len(reclaims) > 0 {
		if err := rotateEventsTx(ctx, tx, now); err != nil {
			return err
		}
	}
	return nil
}

// gcTagsForReclaimTx deletes exactly the reclaimed holder's tag rows. The
// blanket gcTagsTx is WRONG in this tx: its orphan clause deletes any tag
// whose host lock is gone, and in a mixed batch the own-release branch has
// just acked its tags AND deleted its host-lock row a few statements earlier
// — the blanket pass would eat those freshly-acked audit rows before the
// retention window ever saw them (review P2). Keying on the reclaimed row's
// (target, owner, created_at) identity hits only the dead holder's tags.
func gcTagsForReclaimTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM tags WHERE target_canonical = ? AND lock_owner_uuid = ? AND lock_created_at = ?`,
		stale.Target.Canonical, string(stale.OwnerUUID), stale.CreatedAt.UnixNano())
	return err
}

// applyOwnedReleasesTx acks tags, deletes the owned host-lock rows, and emits
// the lock_released events — all in the caller's tx so the row deletes and
// audit stay atomic. Event rotation is hoisted to the caller, which trims once
// for both the own-release and reclaim branches.
func (s *Store) applyOwnedReleasesTx(ctx context.Context, tx *sql.Tx, owned []string, byAgent string, now time.Time) error {
	// Ack tags BEFORE deleting the host locks: the host-lock match must still
	// resolve to set acked_at; if we DELETE first the tags would orphan instead,
	// losing the audit ack (edge #6 distinguishes release-ack from break-orphan).
	if err := ackTagsForReleaseTx(ctx, tx, owned, byAgent); err != nil {
		return err
	}
	if err := deleteOwnedTx(ctx, tx, owned, byAgent); err != nil {
		return err
	}
	// Emit lock_released events in the same tx (atomic with the row deletes).
	evs := make([]domain.Event, len(owned))
	for i, canonical := range owned {
		evs[i] = domain.Event{
			Target:    domain.Target{Canonical: canonical},
			Kind:      EventLockReleased,
			ActorUUID: byAgent,
			CreatedAt: now,
		}
	}
	return appendEventsTx(ctx, tx, evs)
}

// classifyReleases walks input targets in order, classifying each against its
// full holder set: own row → release it (prefer-own; co-holders untouched) ·
// no rows → no-lock · all foreign holders stale → reclaim every holder ·
// ≥1 live foreign holder → not-owner (live-holder veto, loto-w77f). Returns
// the per-target results, the canonicals whose own row gets the batched
// DELETE, and the stale foreign rows to reclaim individually.
func classifyReleases(targets []domain.Target, existing map[string][]domain.LockRecord, byAgent string, ec domain.EvalContext) ([]ReleaseResult, []string, []domain.LockRecord) {
	results := make([]ReleaseResult, len(targets))
	owned := make([]string, 0, len(targets))
	var reclaims []domain.LockRecord
	seen := make(map[string]bool, len(targets))
	for i, t := range targets {
		results[i].Target = t
		if seen[t.Canonical] {
			// Duplicate input target (`unlock a.go a.go`): the first occurrence
			// consumed the row(s), so by this entry's turn there is no lock left
			// — report no-lock rather than re-appending to owned/reclaims, which
			// doubled the lock_released / lock_reclaimed_stale audit (review P3).
			results[i].State = StateNoLock
			continue
		}
		seen[t.Canonical] = true
		holders := existing[t.Canonical]
		switch own := ownHolder(holders, byAgent); {
		case len(holders) == 0:
			results[i].State = StateNoLock
		case own != nil:
			results[i].State = StateUnlocked
			results[i].Mode = own.Mode
			owned = append(owned, t.Canonical)
		case authorizeHolders(holders, ec, false) == nil:
			// The stale-reclaim gate is authorizeHolders — the same single-
			// sourced live-holder veto BreakStale uses (loto-w77f), not a
			// re-derived predicate. All holders share one mode (all-shared-or-
			// one-exclusive invariant), so holders[0] drives restore; holders
			// arrive created_at,owner-ordered, so Owner is deterministic.
			results[i].State = StateReclaimedStale
			results[i].Mode = holders[0].Mode
			results[i].Owner = string(holders[0].OwnerUUID)
			reclaims = append(reclaims, holders...)
		default:
			results[i].State = StateNotOwner
			results[i].Owner = vetoingHolder(holders, ec)
		}
	}
	return results, owned, reclaims
}

// ownHolder returns the caller's row among a target's holders, or nil.
func ownHolder(holders []domain.LockRecord, byAgent string) *domain.LockRecord {
	for i := range holders {
		if string(holders[i].OwnerUUID) == byAgent {
			return &holders[i]
		}
	}
	return nil
}

// vetoingHolder names the first LIVE holder in created_at,owner order — the
// one whose liveness vetoed the stale-reclaim — so the not-owner report points
// at an owner that actually blocks, not an arbitrary (possibly dead) one.
func vetoingHolder(holders []domain.LockRecord, ec domain.EvalContext) string {
	for i := range holders {
		if ec.AuthorizeBreak(holders[i], false) != nil {
			return string(holders[i].OwnerUUID)
		}
	}
	return string(holders[0].OwnerUUID) // unreachable when a veto occurred; deterministic fallback
}

// restoreReleasesThenAudit runs the bounded chmod restore under the still-held
// op-flock, releases the flock, then emits any mode_restore_failed audit off the
// critical section (loto-3qev). Mirrors AcquireLocks' restoreThenReleaseFlock:
// holding the flock across the detached audit's own write tx would extend the
// hold for up to busy_timeout under contention and stall peers on the
// chmod-failure path. The deferred flock.release() at the call site is the
// idempotent backstop.
func (s *Store) restoreReleasesThenAudit(flock *opFlock, results []ReleaseResult, byAgent string) {
	failEvents, failIdx := restoreReleases(results, byAgent)
	flock.release()
	s.auditReleaseFailures(results, failEvents, failIdx)
}

// restoreReleases runs the chmod restore for every released or reclaimed
// EXCLUSIVE target, recording per-target StateRestoreFailed/RestoreErr, and
// returns the mode_restore_failed events plus the parallel result indices.
// Reclaimed targets ride the same pass: their rows are gone from the DB just
// like an own-release, so the file must come back writable under the same
// flock window. Chmod-only half of restoreReleasesThenAudit — runs under the
// held flock (loto-3qev).
func restoreReleases(results []ReleaseResult, byAgent string) ([]domain.Event, []int) {
	now := time.Now()
	var failEvents []domain.Event
	var failIdx []int
	for i := range results {
		if results[i].State != StateUnlocked && results[i].State != StateReclaimedStale {
			continue
		}
		if !shouldRestoreOwnerWrite(results[i].Mode) {
			continue // shared lock never stripped the bit — nothing to restore
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].State = StateRestoreFailed
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
			failIdx = append(failIdx, i)
		}
	}
	return failEvents, failIdx
}

// auditReleaseFailures emits the mode_restore_failed events off the flock
// critical section (loto-3qev); on audit-write failure it fans the error out to
// each affected result so the audit hole is observable (gh#107).
func (s *Store) auditReleaseFailures(results []ReleaseResult, failEvents []domain.Event, failIdx []int) {
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
func (s *Store) ReleaseBySession(ctx context.Context, agent domain.AgentUUID, sessionUUID domain.SessionUUID) ([]ReleaseResult, error) {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
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
	canonicals, err := loadSessionTargetsTx(ctx, tx, byAgent, string(sessionUUID))
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
	// Trim events in the same tx (mirrors AcquireLocks→rotateEventsTx) so a
	// release-heavy workload can't grow the events table unbounded (loto-bvdk).
	if err := rotateEventsTx(ctx, tx, now); err != nil {
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
	s.restoreReleasesThenAudit(flock, results, byAgent)
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
