package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"

	"loto/internal/domain"
)

var (
	ErrNoLockAtTarget    = errors.New("no lock at target")
	ErrTargetSymlink     = errors.New("symlink not supported")
	ErrTargetNotRegular  = errors.New("not a regular file")
	ErrTargetMultiLinked = errors.New("multi-linked file not supported")
)

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

// chmodRestoreErr buffers a per-target restore failure so it can be turned
// into a durable mode_restore_failed tag AFTER the acquire tx rolls back.
type chmodRestoreErr struct {
	path string
	err  error
}

// AcquireLocks atomically acquires locks on all targets. Either all targets
// are stripped-write + DB rows inserted, or none are (with chmod rollback).
//
// If the process dies between the chmod loop and tx.Commit, files are stripped
// with no DB row — exactly the orphan-mode case `doctor` is designed to surface.
func (s *Store) AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	sorted := make([]domain.LockRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Target.Canonical < sorted[j].Target.Canonical
	})

	flock, err := acquireOpFlock(s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	if err := validateFileTargets(sorted); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	caseSensitive, err := s.fsCaseSensitiveTx(tx)
	if err != nil {
		return nil, err
	}
	caseInsensitive := !caseSensitive

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	blockers, err := collectAllBlockers(ctx, tx, all, sorted, caseInsensitive, now, live)
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
	for _, re := range restoreErrs {
		_ = s.appendModeRestoreFailedTag(ctx, re.path, sorted[0].OwnerUUID, now, re.err)
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

func validateFileTargets(sorted []domain.LockRecord) error {
	for i := range sorted {
		if sorted[i].Target.Kind != domain.KindFile {
			continue
		}
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

func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, caseInsensitive bool, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	seen := map[string]bool{}
	var blockers []domain.LockRecord
	for i := range sorted {
		bs, err := collectBlockers(ctx, tx, all, sorted[i], caseInsensitive, now, live)
		if err != nil {
			return nil, err
		}
		for i := range bs {
			key := bs[i].OwnerUUID + "|" + bs[i].Target.Canonical
			if !seen[key] {
				seen[key] = true
				blockers = append(blockers, bs[i])
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

// stripAll chmods write off each KindFile target in canonical order. On the
// first failure, it returns the partial stripped list plus a ChmodFailure
// describing the offending target. Dir/glob targets are skipped (logical-only).
func stripAll(sorted []domain.LockRecord) ([]string, *ChmodFailure) {
	stripped := make([]string, 0, len(sorted))
	for i := range sorted {
		if sorted[i].Target.Kind != domain.KindFile {
			continue
		}
		p := sorted[i].Target.Canonical
		if err := stripWrite(p); err != nil {
			return stripped, &ChmodFailure{Target: sorted[i].Target, Err: err}
		}
		stripped = append(stripped, p)
	}
	return stripped, nil
}

// rollbackStripped reverses successful strips after a partial failure.
// Returns the failure list (initial failure first, then rollback outcomes)
// and any restore errors needing durable mode_restore_failed breadcrumbs.
func rollbackStripped(failedTarget domain.Target, failedErr error, stripped []string) ([]ChmodFailure, []chmodRestoreErr) {
	failures := []ChmodFailure{{Target: failedTarget, Err: failedErr, RolledBack: false}}
	var restoreErrs []chmodRestoreErr
	for _, p := range stripped {
		if rerr := restoreWrite(p); rerr != nil {
			failures = append(failures, ChmodFailure{
				Target:     domain.Target{Canonical: p, Kind: domain.KindFile},
				Err:        rerr,
				RolledBack: false,
			})
			restoreErrs = append(restoreErrs, chmodRestoreErr{path: p, err: rerr})
		} else {
			failures = append(failures, ChmodFailure{
				Target:     domain.Target{Canonical: p, Kind: domain.KindFile},
				RolledBack: true,
			})
		}
	}
	return failures, restoreErrs
}

// appendModeRestoreFailedTag writes the durable breadcrumb on its own
// connection. Callers MUST have rolled back the surrounding acquire tx first.
func (s *Store) appendModeRestoreFailedTag(ctx context.Context, path, byAgent string, now time.Time, cause error) error {
	tagID := newTagID(byAgent, now, "mode_restore_failed")
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tags(target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		path, "file", tagID, "system", "mode_restore_failed",
		byAgent, byAgent, "",
		fmt.Sprintf("mode_restore_failed: %v on %s", cause, path),
		now.UnixNano(),
	)
	return err
}

func collectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, caseInsensitive bool, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	var blockers []domain.LockRecord
	for i := range all {
		ex := &all[i]
		if !domain.Overlap(ex.Target, l.Target, caseInsensitive) || ex.OwnerUUID == l.OwnerUUID {
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
INSERT INTO locks(target_canonical, target_kind, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, branch)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  branch=excluded.branch
WHERE locks.owner_uuid = excluded.owner_uuid`,
		l.Target.Canonical, kindString(l.Target.Kind), l.OwnerUUID, l.SessionUUID,
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, l.Branch,
	)
	return err
}

func (s *Store) ReleaseLock(ctx context.Context, t domain.Target, byAgent string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, t.Canonical, byAgent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotOwner
	}
	return tx.Commit()
}

func (s *Store) BreakLock(ctx context.Context, t domain.Target, byAgent string, force bool, reason string, live domain.PidLiveProbe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT `+lockCols+` FROM locks WHERE target_canonical = ?`, t.Canonical)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNoLockAtTarget
	}
	l, err := scanLock(rows)
	if err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	now := time.Now()
	if err := domain.AuthorizeBreak(l, byAgent, force, now, l.Host, live); err != nil {
		return err
	}

	event := "lock_broken"
	if !force {
		event = "lock_reclaimed_stale"
	}
	tagID := newTagID(byAgent, now, reason)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tags(target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		t.Canonical, kindString(l.Target.Kind), tagID, "system", event, byAgent, l.OwnerUUID, l.OwnerUUID, reason, now.UnixNano(),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, t.Canonical, l.OwnerUUID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListLocks(ctx context.Context) ([]domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lockCols+` FROM locks`)
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

func (s *Store) LockAt(ctx context.Context, t domain.Target) (*domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lockCols+` FROM locks WHERE target_canonical = ?`, t.Canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil //nolint:nilnil // (nil, nil) signals "no row"; explicit not-found
	}
	l, err := scanLock(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &l, nil
}

const lockCols = `target_canonical,target_kind,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,branch`

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
	var canonical, kindStr string
	var createdNs, expiresNs int64
	if err := r.Scan(&canonical, &kindStr, &l.OwnerUUID, &l.SessionUUID, &l.Intent, &createdNs, &expiresNs, &l.Host, &l.PID, &l.Branch); err != nil {
		return l, err
	}
	l.Target = domain.Target{Canonical: canonical, Kind: parseKind(kindStr)}
	l.CreatedAt = time.Unix(0, createdNs).UTC()
	l.ExpiresAt = time.Unix(0, expiresNs).UTC()
	return l, nil
}

func kindString(k domain.TargetKind) string {
	switch k {
	case domain.KindFile:
		return "file"
	case domain.KindDir:
		return "dir"
	case domain.KindGlob:
		return "glob"
	}
	return "file"
}

func parseKind(s string) domain.TargetKind {
	switch s {
	case "dir":
		return domain.KindDir
	case "glob":
		return domain.KindGlob
	default:
		return domain.KindFile
	}
}

func reclaimStaleTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord, byAgent string, now time.Time) error {
	tagID := newTagID(byAgent, now, "lock_reclaimed_stale")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tags(target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		stale.Target.Canonical, kindString(stale.Target.Kind), tagID, "system", "lock_reclaimed_stale",
		byAgent, stale.OwnerUUID, stale.OwnerUUID,
		"reclaimed stale lock", now.UnixNano(),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, stale.Target.Canonical, stale.OwnerUUID); err != nil {
		return err
	}
	return nil
}

func (s *Store) fsCaseSensitiveTx(tx *sql.Tx) (bool, error) {
	var v string
	err := tx.QueryRowContext(context.Background(), `SELECT value FROM schema_meta WHERE key = 'fs_case_sensitive'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return v == "true", nil
}
