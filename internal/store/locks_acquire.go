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

func (s *Store) AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	sorted := make([]domain.LockRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Target.Canonical < sorted[j].Target.Canonical
	})

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

	blockers, err := collectAllBlockers(ctx, tx, all, sorted, now, live)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return nil, &MultiConflictError{Blockers: blockers}
	}

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
		s.restoreAllAndAudit(ctx, stripped, sorted[0].OwnerUUID, now)
		return nil, err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		cleanup()
		s.restoreAllAndAudit(ctx, stripped, sorted[0].OwnerUUID, now)
		return nil, err
	}
	if err := commitTxFn(tx); err != nil {
		cleanup()
		s.restoreAllAndAudit(ctx, stripped, sorted[0].OwnerUUID, now)
		return nil, err
	}
	return sorted, nil
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
		evs = append(evs, modeRestoreFailedEvent(re.path, sorted[0].OwnerUUID, now, re.err))
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
			ActorUUID: sorted[i].OwnerUUID,
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

func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	// Bundle the (now, live) ambient pair once; ThisHost is set per-lock inside
	// reclaimStaleAndCollectBlockers, where the acquiring lock's host is known.
	ec := domain.EvalContext{Now: now, Live: live}
	seen := map[string]bool{}
	var blockers []domain.LockRecord
	for i := range sorted {
		bs, err := reclaimStaleAndCollectBlockers(ctx, tx, all, sorted[i], ec)
		if err != nil {
			return nil, err
		}
		for j := range bs {
			key := bs[j].OwnerUUID + "|" + bs[j].Target.Canonical
			if !seen[key] {
				seen[key] = true
				blockers = append(blockers, bs[j])
			}
		}
	}
	sort.Slice(blockers, func(i, j int) bool {
		if !blockers[i].CreatedAt.Equal(blockers[j].CreatedAt) {
			return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
		}
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	return blockers, nil
}

func stripAll(sorted []domain.LockRecord) ([]string, *ChmodFailure) {
	stripped := make([]string, 0, len(sorted))
	for i := range sorted {
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

func reclaimStaleAndCollectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, ec domain.EvalContext) ([]domain.LockRecord, error) {
	ec = ec.WithHost(l.Host)
	var blockers []domain.LockRecord
	for i := range all {
		ex := &all[i]
		if !domain.Overlap(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID {
			continue
		}
		if ec.IsStale(*ex) {
			if err := reclaimStaleTx(ctx, tx, *ex, l.OwnerUUID, ec.Now); err != nil {
				return nil, err
			}
			continue
		}
		blockers = append(blockers, all[i])
	}
	return blockers, nil
}

func insertOrRefreshLock(ctx context.Context, tx *sql.Tx, l domain.LockRecord) error {
	// Map 0 (UNKNOWN) → NULL at the store boundary so an absent start-time is a
	// SQL null, matching legacy rows. A refresh re-stamps proc_start because the
	// holder is the same process (same pid, same start-time).
	var procStart any
	if l.ProcStart != 0 {
		procStart = l.ProcStart
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, proc_start, branch)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  proc_start=excluded.proc_start,
  branch=excluded.branch
WHERE locks.owner_uuid = excluded.owner_uuid`,
		l.Target.Canonical, l.OwnerUUID, l.SessionUUID,
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, procStart, l.Branch,
	)
	return err
}
