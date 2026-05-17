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

	stripped, chmodFailErr := s.stripAndHandleFailure(ctx, tx, sorted, now)
	if chmodFailErr != nil {
		return nil, chmodFailErr
	}

	if err := insertAllLocks(ctx, tx, sorted, stripped); err != nil {
		return nil, err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		restoreAll(stripped)
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		restoreAll(stripped)
		return nil, err
	}
	return sorted, nil
}

func (s *Store) stripAndHandleFailure(ctx context.Context, tx *sql.Tx, sorted []domain.LockRecord, now time.Time) ([]string, error) {
	stripped, chmodErr := stripAll(sorted)
	if chmodErr == nil {
		return stripped, nil
	}
	failures, restoreErrs := rollbackStripped(chmodErr.Target, chmodErr.Err, stripped)
	_ = tx.Rollback()
	if len(restoreErrs) > 0 {
		evs := make([]domain.Event, 0, len(restoreErrs))
		for _, re := range restoreErrs {
			evs = append(evs, modeRestoreFailedEvent(re.path, sorted[0].OwnerUUID, now, re.err))
		}
		_ = s.AppendEvents(ctx, evs)
	}
	return nil, &ChmodFailureError{Failures: failures}
}

func insertAllLocks(ctx context.Context, tx *sql.Tx, sorted []domain.LockRecord, stripped []string) error {
	for i := range sorted {
		if err := insertOrRefreshLock(ctx, tx, sorted[i]); err != nil {
			restoreAll(stripped)
			return err
		}
	}
	return nil
}

func restoreAll(stripped []string) {
	for _, p := range stripped {
		_ = restoreWrite(p)
	}
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
		return fmt.Errorf("validate %s: %w", p, ErrTargetSymlink)
	}
	if !lst.Mode().IsRegular() {
		return fmt.Errorf("validate %s: %w", p, ErrTargetNotRegular)
	}
	if sys, ok := lst.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return fmt.Errorf("validate %s (Nlink=%d): %w", p, sys.Nlink, ErrTargetMultiLinked)
	}
	return nil
}

func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	seen := map[string]bool{}
	var blockers []domain.LockRecord
	for i := range sorted {
		bs, err := reclaimStaleAndCollectBlockers(ctx, tx, all, sorted[i], now, live)
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

func (s *Store) appendModeRestoreFailedEvent(ctx context.Context, path, byAgent string, now time.Time, cause error) error {
	_, err := s.AppendEvent(ctx, domain.Event{
		Target:    domain.Target{Canonical: path},
		Kind:      EventModeRestoreFailed,
		ActorUUID: byAgent,
		Reason:    fmt.Sprintf("mode_restore_failed: %v on %s", cause, path),
		CreatedAt: now,
	})
	return err
}

func reclaimStaleAndCollectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	var blockers []domain.LockRecord
	for i := range all {
		ex := &all[i]
		if !domain.Overlap(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID {
			continue
		}
		if domain.IsStale(*ex, now, l.Host, live) {
			if err := reclaimStaleTx(ctx, tx, *ex, l.OwnerUUID, now); err != nil {
				return nil, err
			}
			continue
		}
		blockers = append(blockers, all[i])
	}
	return blockers, nil
}

func insertOrRefreshLock(ctx context.Context, tx *sql.Tx, l domain.LockRecord) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, branch)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  branch=excluded.branch
WHERE locks.owner_uuid = excluded.owner_uuid`,
		l.Target.Canonical, l.OwnerUUID, l.SessionUUID,
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, l.Branch,
	)
	return err
}
