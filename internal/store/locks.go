package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"loto/internal/domain"
)

const (
	EventLockAcquired         = "lock_acquired"
	EventLockReleased         = "lock_released"
	EventLockBroken           = "lock_broken"
	EventLockReclaimedStale   = "lock_reclaimed_stale"
	EventModeRestoreFailed    = "mode_restore_failed"
	EventAcquireRollbackStart = "acquire_rollback_started"
)

var ErrNoLockAtTarget = errors.New("no lock at target")

// TargetValidationError reports why a target path can't be locked. Replaces
// the prior bare-sentinel design: ErrTargetMultiLinked used to smuggle Nlink
// through a %d in fmt.Errorf's wrap message — the sentinel could be matched
// via errors.Is but Nlink couldn't be recovered. Holding Path + Nlink on the
// struct preserves state across the error boundary.
type TargetValidationError struct {
	Path   string
	Reason TargetValidationReason
	Nlink  uint64 // populated for ReasonMultiLinked, zero otherwise
}

// TargetValidationReason discriminates the failure modes.
type TargetValidationReason int

const (
	ReasonSymlink TargetValidationReason = iota
	ReasonNotRegular
	ReasonMultiLinked
)

func (e *TargetValidationError) Error() string {
	switch e.Reason {
	case ReasonSymlink:
		return fmt.Sprintf("validate %s: symlink not supported", e.Path)
	case ReasonNotRegular:
		return fmt.Sprintf("validate %s: not a regular file", e.Path)
	case ReasonMultiLinked:
		return fmt.Sprintf("validate %s (Nlink=%d): multi-linked file not supported", e.Path, e.Nlink)
	default:
		return fmt.Sprintf("validate %s: unknown validation failure", e.Path)
	}
}

// MultiConflictError aggregates blockers across multiple targets.
type MultiConflictError struct {
	Blockers []domain.LockRecord
}

func (e *MultiConflictError) Error() string {
	return fmt.Sprintf("multi-target lock conflict: %d blocker(s)", len(e.Blockers))
}

// ChmodFailure describes a single target's chmod outcome during a failed
// multi-acquire. RolledBack=true means the strip was successfully reversed.
type ChmodFailure struct {
	Target     domain.Target
	Err        error
	RolledBack bool
}

type ChmodFailureError struct {
	Failures []ChmodFailure
}

func (e *ChmodFailureError) Error() string {
	return fmt.Sprintf("chmod failed on %d target(s)", len(e.Failures))
}

type chmodRestoreErr struct {
	path string
	err  error
}

// ReleaseOutcome distinguishes the per-target result of a multi-target release.
type ReleaseOutcome int

const (
	// StateUnlocked: row deleted and chmod restore succeeded.
	StateUnlocked ReleaseOutcome = iota
	// StateNoLock: no row at target — caller wasn't holding it.
	StateNoLock
	// StateNotOwner: row exists but owned by another agent.
	StateNotOwner
	// StateRestoreFailed: row deleted, chmod restore failed.
	StateRestoreFailed
)

// ReleaseResult is the per-target outcome from ReleaseLocks.
type ReleaseResult struct {
	Target     domain.Target
	State      ReleaseOutcome
	Holder     string // populated when State == StateNotOwner
	RestoreErr error  // populated when State == StateRestoreFailed
}

// BreakResult is the per-target outcome from BreakLocks. Err is nil on success;
// ErrNoLockAtTarget or an AuthorizeBreak error otherwise. RestoreErr is set
// independently when the lock row was deleted but post-commit chmod-restore
// failed — the break itself succeeded but the file is left read-only, mirroring
// ReleaseResult.StateRestoreFailed semantics. Restore failures are also audited
// via mode_restore_failed events.
type BreakResult struct {
	Target     domain.Target
	Err        error
	RestoreErr error
}

const lockCols = `target_canonical,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,branch`

func inClause(targets []domain.Target) (string, []any) {
	ph := make([]byte, 0, len(targets)*2)
	args := make([]any, 0, len(targets))
	for i, t := range targets {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, t.Canonical)
	}
	return string(ph), args
}

func inClauseStrings(ss []string) (string, []any) {
	ph := make([]byte, 0, len(ss)*2)
	args := make([]any, 0, len(ss))
	for i, s := range ss {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, s)
	}
	return string(ph), args
}

func modeRestoreFailedEvent(path, byAgent string, now time.Time, cause error) domain.Event {
	return domain.Event{
		Target:    domain.Target{Canonical: path},
		Kind:      EventModeRestoreFailed,
		ActorUUID: byAgent,
		Reason:    fmt.Sprintf("mode_restore_failed: %v on %s", cause, path),
		CreatedAt: now,
	}
}

func loadLocksTx(ctx context.Context, tx *sql.Tx) ([]domain.LockRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+lockCols+` FROM locks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LockRecord
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLock(r *sql.Rows) (domain.LockRecord, error) {
	var l domain.LockRecord
	var canonical string
	var createdNs, expiresNs int64
	if err := r.Scan(&canonical, &l.OwnerUUID, &l.SessionUUID, &l.Intent, &createdNs, &expiresNs, &l.Host, &l.PID, &l.Branch); err != nil {
		return l, err
	}
	l.Target = domain.Target{Canonical: canonical}
	l.CreatedAt = time.Unix(0, createdNs).UTC()
	l.ExpiresAt = time.Unix(0, expiresNs).UTC()
	return l, nil
}

func reclaimStaleTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord, byAgent string, now time.Time) error {
	if err := appendEventTx(ctx, tx, domain.Event{
		Target:      stale.Target,
		Kind:        EventLockReclaimedStale,
		ActorUUID:   byAgent,
		SubjectUUID: stale.OwnerUUID,
		Reason:      "reclaimed stale lock",
		CreatedAt:   now,
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, stale.Target.Canonical, stale.OwnerUUID); err != nil {
		return err
	}
	return nil
}
