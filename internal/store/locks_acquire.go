package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"

	"loto/internal/domain"
)

// sortedByCanonical returns a copy of recs ordered by canonical target path,
// the deterministic lock-acquisition order that prevents ABBA deadlocks.
func sortedByCanonical(recs []domain.LockRecord) []domain.LockRecord {
	sorted := make([]domain.LockRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Target.Canonical < sorted[j].Target.Canonical
	})
	return sorted
}

// AcquireLocks acquires a batch of locks in one transaction under the project
// op-flock.
//
// PRECONDITION: every record in recs must carry the SAME OwnerUUID. The reclaim
// / restore / rollback audit breadcrumbs (acquire_rollback_started,
// mode_restore_failed) attribute the whole batch to sorted[0].OwnerUUID, so a
// mixed-owner batch would name the wrong actor on those events (loto-13pk). The
// loto CLI always builds single-owner batches (one agent per invocation via
// buildLockRecords from rt.Agent.UUID), so the precondition holds today; a
// future batch-import/migration caller that submits mixed owners must thread
// the per-record owner through stripped/reclaimed before relying on these
// audit events.
func (s *Store) AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	sorted := sortedByCanonical(recs)

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	if err := validateAllFileTargets(sorted); err != nil {
		return nil, err
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	blockers, reclaimed, err := collectAllBlockers(ctx, tx, all, sorted, now, live)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return nil, &MultiConflictError{Blockers: blockers}
	}

	// Same-owner exclusive→shared re-acquire downgrades the existing row in place
	// (insertOrRefreshLock upserts mode=shared). The original exclusive acquire
	// stripped owner-write; the shared upsert never restores it and stripAll skips
	// shared rows, so without this the owner's own file stays read-only forever
	// (loto-h760). Collect these paths from the pre-acquire snapshot and restore
	// the bit post-commit, mirroring DowngradeLock's restore semantics.
	downgraded := collectSameOwnerDowngrades(all, sorted)

	stripped, chmodFailErr := s.stripAndHandleFailure(tx, sorted, now)
	if chmodFailErr != nil {
		return nil, chmodFailErr
	}

	// On any failure from here on, the parent tx still holds the SQLite write
	// lock. Release it via cleanup() BEFORE restoreAllAndAudit — the detached
	// audit opens its own write tx, which would otherwise self-contend with
	// the held lock and stall ~2s on busy_timeout, dropping the breadcrumb
	// (loto-rmyg). cleanup() is idempotent, so the deferred call is harmless.
	if err := s.insertAllLocks(ctx, tx, sorted, now); err != nil {
		cleanup()
		s.restoreAllAndAudit(ctx, stripped, string(sorted[0].OwnerUUID), now)
		return nil, err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		cleanup()
		s.restoreAllAndAudit(ctx, stripped, string(sorted[0].OwnerUUID), now)
		return nil, err
	}
	if err := commitTxFn(tx); err != nil {
		cleanup()
		s.restoreAllAndAudit(ctx, stripped, string(sorted[0].OwnerUUID), now)
		return nil, err
	}

	// Post-commit FS restore. Two opposing constraints meet here:
	//
	//   - loto-v8ch/loto-4qt (correctness, P1): the chmod restore MUST run while
	//     the op-flock is held. The DB now says the reclaimed stale rows are gone;
	//     if a peer takes the flock mid-restore it can read the consistent DB and
	//     either see a target still chmod read-only (torn row+file view), or — the
	//     worse interleaving — acquire exclusive and re-strip, after which our
	//     restoreWrite re-adds owner-write under the peer's lease and silently
	//     defeats its exclusivity (the restripped skip-set covers only OUR strips,
	//     not the peer's). This is the same silent-clobber Break/Doctor hold the
	//     flock to prevent.
	//
	//   - loto-9uy5 (perf, P3, no correctness loss): the detached AUDIT write must
	//     NOT run under the flock. Its beginTx opens a fresh write tx that, under
	//     cross-process contention, can block up to busy_timeout (~2s) and extend
	//     the flock hold, stalling every other loto invocation on this repo.
	//
	// Resolution: do the chmod restore HERE, under the held flock (bounded, fast —
	// just fchmod). It returns the fail-events; we release the flock, THEN emit the
	// audit off the critical section. Correctness (v8ch) and the anti-stall goal
	// (9uy5) are both satisfied — the audit, not the chmod, was 9uy5's stall source.
	//
	// Failure paths above roll the tx back, which reinstates the reclaimed stale
	// rows — so the file correctly stays stripped there. Only after a successful
	// commit are the stale rows truly gone and the restore due.
	s.restoreThenReleaseFlock(flock, append(reclaimed, downgraded...), stripped, string(sorted[0].OwnerUUID), now)
	return sorted, nil
}

// collectSameOwnerDowngrades returns the canonical paths where an existing
// same-owner EXCLUSIVE row is being re-acquired as SHARED. insertOrRefreshLock
// flips the row to shared in place, but nothing restores the owner-write bit the
// original exclusive acquire stripped (stripAll skips shared incoming rows, and
// the same-owner row is never a reclaim/break candidate). Restoring these paths
// post-commit mirrors DowngradeLock (loto-h760). Scoped strictly to same-owner
// downgrades — other-owner rows are untouched.
func collectSameOwnerDowngrades(all []domain.LockRecord, sorted []domain.LockRecord) []string {
	var downgraded []string
	for i := range sorted {
		if sorted[i].EffectiveMode() != domain.ModeShared {
			continue // incoming is exclusive — stripAll/normal flow handles it
		}
		for j := range all {
			ex := &all[j]
			if ex.OwnerUUID != sorted[i].OwnerUUID ||
				!domain.SameCanonical(ex.Target, sorted[i].Target) {
				continue
			}
			if ex.EffectiveMode() == domain.ModeExclusive {
				downgraded = append(downgraded, sorted[i].Target.Canonical)
			}
			break // composite PK (target, owner) → at most one match
		}
	}
	return downgraded
}

// restoreThenReleaseFlock runs the bounded chmod restore under the still-held
// op-flock, releases the flock, then emits any mode_restore_failed audit off the
// critical section. Splitting it this way satisfies both loto-v8ch (chmod under
// the flock) and loto-9uy5 (audit write tx not under the flock); see the call
// site for the full rationale. The deferred flock.release() in AcquireLocks is
// the idempotent backstop on the failure paths above.
func (s *Store) restoreThenReleaseFlock(flock *opFlock, reclaimed, stripped []string, byAgent string, now time.Time) {
	failEvents := restoreReclaimedSkippingRestripped(reclaimed, stripped, byAgent, now)
	flock.release()
	if len(failEvents) > 0 {
		_ = s.appendAuditDetached(failEvents)
	}
}

// restoreReclaimedSkippingRestripped re-adds owner-write to paths whose stale
// EXCLUSIVE rows were reclaimed during this acquire (the stale holder stripped
// the bit and is gone; nothing else would ever restore it, loto-22ka) — except
// paths this acquire itself re-stripped: an exclusive acquirer on the same
// target keeps the bit off. Runs post-commit; mirrors DoctorRepair's
// restoreReclaimedAndAudit (doctor.go).
//
// This is the chmod-only half — the CALLER runs it under the held op-flock
// (loto-v8ch) and emits the returned mode_restore_failed events AFTER releasing
// the flock, so the detached audit's write tx can't extend the flock hold
// (loto-9uy5). Returning the events instead of writing them here is what keeps
// the audit off the flock critical section.
func restoreReclaimedSkippingRestripped(reclaimed, stripped []string, byAgent string, now time.Time) []domain.Event {
	if len(reclaimed) == 0 {
		return nil
	}
	restripped := make(map[string]bool, len(stripped))
	for _, p := range stripped {
		restripped[p] = true
	}
	var failEvents []domain.Event
	for _, p := range reclaimed {
		if restripped[p] {
			continue
		}
		if rerr := restoreWrite(p); rerr != nil {
			failEvents = append(failEvents, modeRestoreFailedEvent(p, byAgent, now, rerr))
		}
	}
	return failEvents
}

// restoreAllAndAudit restores write bits on every stripped path. Emits
// an acquire_rollback_started breadcrumb BEFORE the chmod loop so a
// mid-loop crash leaves a durable trail pointing at the orphan-mode
// files; per-path mode_restore_failed events follow for any restore
// that fails (gh#122).
//
// Audit writes use a detached bounded ctx so an already-cancelled
// caller ctx doesn't scale busy_timeout to ~1ms and silently drop the
// trail — the post-commit/post-rollback restore is the one moment we
// most need the audit to land.
func (s *Store) restoreAllAndAudit(_ context.Context, stripped []string, byAgent string, now time.Time) {
	if len(stripped) == 0 {
		return
	}
	start := []domain.Event{{
		Target:    domain.Target{Canonical: stripped[0]},
		Kind:      EventAcquireRollbackStart,
		ActorUUID: byAgent,
		Reason:    fmt.Sprintf("acquire_rollback_started: restoring %d path(s); first=%s", len(stripped), stripped[0]),
		CreatedAt: now,
	}}
	_ = s.appendAuditDetached(start)

	var evs []domain.Event
	for _, p := range stripped {
		if err := restoreWrite(p); err != nil {
			evs = append(evs, modeRestoreFailedEvent(p, byAgent, now, err))
		}
	}
	if len(evs) > 0 {
		_ = s.appendAuditDetached(evs)
	}
}

func (s *Store) stripAndHandleFailure(tx *sql.Tx, sorted []domain.LockRecord, now time.Time) ([]string, error) {
	stripped, chmodErr := stripAll(sorted)
	if chmodErr == nil {
		return stripped, nil
	}
	failures, restoreErrs := rollbackStripped(chmodErr.Target, chmodErr.Err, stripped)
	if len(restoreErrs) == 0 {
		_ = tx.Rollback()
		return nil, &ChmodFailureError{Failures: failures}
	}
	// Persist restore-failure audits IN-TX before committing — the parent tx
	// has only run SELECTs so a write+commit makes the audit atomic with the
	// failed acquire (gh#107). On any in-tx error, fall back to the detached
	// path which logs to s.stderr so the loss is observable.
	evs := make([]domain.Event, 0, len(restoreErrs))
	for _, re := range restoreErrs {
		evs = append(evs, modeRestoreFailedEvent(re.path, string(sorted[0].OwnerUUID), now, re.err))
	}
	auditCtx, cancel := context.WithTimeout(context.Background(), auditDetachedTimeout)
	defer cancel()
	if err := appendEventsTx(auditCtx, tx, evs); err != nil {
		_ = tx.Rollback()
		_ = s.appendAuditDetached(evs)
	} else if err := tx.Commit(); err != nil {
		_ = s.appendAuditDetached(evs)
	}
	return nil, &ChmodFailureError{Failures: failures}
}

// insertAllLocks writes the lock rows and their lock_acquired events inside
// the parent tx. On error the caller (AcquireLocks) releases the tx and runs
// restoreAllAndAudit, so failures here just propagate the error.
func (s *Store) insertAllLocks(ctx context.Context, tx *sql.Tx, sorted []domain.LockRecord, now time.Time) error {
	for i := range sorted {
		if err := insertOrRefreshLock(ctx, tx, sorted[i]); err != nil {
			return err
		}
	}
	// Emit lock_acquired events in the same tx (atomic with the row inserts).
	evs := make([]domain.Event, len(sorted))
	for i := range sorted {
		evs[i] = domain.Event{
			Target:    sorted[i].Target,
			Kind:      EventLockAcquired,
			ActorUUID: string(sorted[i].OwnerUUID),
			Reason:    sorted[i].Intent,
			CreatedAt: now,
		}
	}
	return appendEventsTx(ctx, tx, evs)
}

func validateAllFileTargets(sorted []domain.LockRecord) error {
	for i := range sorted {
		if err := validateFileTarget(sorted[i].Target.Canonical); err != nil {
			return err
		}
	}
	return nil
}

func validateFileTarget(p string) error {
	lst, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("validate %s: %w", p, err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return &TargetValidationError{Path: p, Reason: ReasonSymlink}
	}
	if !lst.Mode().IsRegular() {
		return &TargetValidationError{Path: p, Reason: ReasonNotRegular}
	}
	if sys, ok := lst.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return &TargetValidationError{Path: p, Reason: ReasonMultiLinked, Nlink: uint64(sys.Nlink)}
	}
	return nil
}

// collectAllBlockers returns the live conflicting holders plus the canonical
// paths of reclaimed stale EXCLUSIVE rows (deduped) — the caller must restore
// owner-write on those after commit unless it re-stripped them itself.
func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, []string, error) {
	// Bundle the (now, live) ambient pair once; ThisHost is set per-lock inside
	// reclaimStaleAndCollectBlockers, where the acquiring lock's host is known.
	ec := domain.EvalContext{Now: now, Live: live}
	seen := map[string]bool{}
	seenReclaimed := map[string]bool{}
	var blockers []domain.LockRecord
	var reclaimed []string
	for i := range sorted {
		bs, rc, err := reclaimStaleAndCollectBlockers(ctx, tx, all, sorted[i], ec)
		if err != nil {
			return nil, nil, err
		}
		for j := range bs {
			key := string(bs[j].OwnerUUID) + "|" + bs[j].Target.Canonical
			if !seen[key] {
				seen[key] = true
				blockers = append(blockers, bs[j])
			}
		}
		for _, p := range rc {
			if !seenReclaimed[p] {
				seenReclaimed[p] = true
				reclaimed = append(reclaimed, p)
			}
		}
	}
	sort.Slice(blockers, func(i, j int) bool {
		if !blockers[i].CreatedAt.Equal(blockers[j].CreatedAt) {
			return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
		}
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	return blockers, reclaimed, nil
}

func stripAll(sorted []domain.LockRecord) ([]string, *ChmodFailure) {
	stripped := make([]string, 0, len(sorted))
	for i := range sorted {
		if sorted[i].EffectiveMode() != domain.ModeExclusive {
			continue // shared locks are advisory-only; write bit untouched
		}
		p := sorted[i].Target.Canonical
		if err := stripWrite(p); err != nil {
			return stripped, &ChmodFailure{Target: sorted[i].Target, Err: err}
		}
		stripped = append(stripped, p)
	}
	return stripped, nil
}

func rollbackStripped(failedTarget domain.Target, failedErr error, stripped []string) ([]ChmodFailure, []chmodRestoreErr) {
	failures := []ChmodFailure{{Target: failedTarget, Err: failedErr, RolledBack: false}}
	var restoreErrs []chmodRestoreErr
	for _, p := range stripped {
		if rerr := restoreWrite(p); rerr != nil {
			failures = append(failures, ChmodFailure{
				Target:     domain.Target{Canonical: p},
				Err:        rerr,
				RolledBack: false,
			})
			restoreErrs = append(restoreErrs, chmodRestoreErr{path: p, err: rerr})
		} else {
			failures = append(failures, ChmodFailure{
				Target:     domain.Target{Canonical: p},
				RolledBack: true,
			})
		}
	}
	return failures, restoreErrs
}

// reclaimStaleAndCollectBlockers deletes stale rows contending with l and
// returns the surviving blockers, plus the canonical paths of reclaimed rows
// that had stripped owner-write (stale EXCLUSIVE holders — per the
// shouldRestoreOwnerWrite guard, locks.go) so the caller can restore the bit
// once the deletes commit.
func reclaimStaleAndCollectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, ec domain.EvalContext) ([]domain.LockRecord, []string, error) {
	ec = ec.WithHost(l.Host)
	var blockers []domain.LockRecord
	var reclaimed []string
	for i := range all {
		ex := &all[i]
		if !domain.SameCanonical(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID {
			continue
		}
		if ec.IsStale(*ex) {
			if err := reclaimStaleTx(ctx, tx, *ex, string(l.OwnerUUID), ec.Now); err != nil {
				return nil, nil, err
			}
			if shouldRestoreOwnerWrite(ex.Mode) {
				reclaimed = append(reclaimed, ex.Target.Canonical)
			}
			continue
		}
		// Mode-aware: a shared peer does not block a shared acquire. The
		// same-canonical/same-owner/stale guards above are kept for the reclaim
		// side-effect; Conflicts is the final gate on whether a live, non-self
		// peer actually blocks (loto-k5el.2 T3).
		if ec.Conflicts(l, *ex) {
			blockers = append(blockers, all[i])
		}
	}
	return blockers, reclaimed, nil
}

func insertOrRefreshLock(ctx context.Context, tx *sql.Tx, l domain.LockRecord) error {
	// Map 0 (UNKNOWN) → NULL at the store boundary so an absent start-time is a
	// SQL null, matching legacy rows. A refresh re-stamps proc_start because the
	// holder is the same process (same pid, same start-time).
	var procStart any
	if l.ProcStart != 0 {
		procStart = l.ProcStart
	}
	// ON CONFLICT targets the composite PK (target_canonical, owner_uuid) added in
	// loto-k5el.2 — so a same-owner re-acquire upserts its single row while a
	// different owner inserts a coexisting row (multi-holder). The old
	// `WHERE locks.owner_uuid = excluded.owner_uuid` guard is now redundant (the
	// conflict is keyed on owner) and dropped. Persist EffectiveMode() (not raw
	// l.Mode) so the column never stores '' (loto-k5el.2 T3).
	_, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, proc_start, branch, mode)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical, owner_uuid) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  proc_start=excluded.proc_start,
  branch=excluded.branch,
  mode=excluded.mode`,
		l.Target.Canonical, string(l.OwnerUUID), l.SessionUUID,
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, procStart, l.Branch, l.EffectiveMode(),
	)
	return err
}
