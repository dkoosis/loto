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

const (
	EventLockAcquired       = "lock_acquired"
	EventLockReleased       = "lock_released"
	EventLockBroken         = "lock_broken"
	EventLockReclaimedStale = "lock_reclaimed_stale"
	EventModeRestoreFailed  = "mode_restore_failed"
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

type chmodRestoreErr struct {
	path string
	err  error
}

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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	// collectAllBlockers also performs lazy GC of stale rows. Reclaimed paths
	// are not chmod-restored here: collectBlockers only deletes rows that
	// overlap the requested locks, and overlapping new locks immediately
	// re-strip the same paths. Orphan-mode (stale row reclaimed with no new
	// overlap) cannot occur via this code path; doctor's stale-scan handles
	// non-overlapping orphans.
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
		bs, err := collectBlockers(ctx, tx, all, sorted[i], now, live)
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

func collectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, now time.Time, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
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

// ReleaseLocks releases each target best-effort under the project op-flock in
// a single transaction (SELECT … WHERE IN, batched DELETE). Returns one
// ReleaseResult per input target in input order — render owns the canonical
// sort for stable output. The returned error is non-nil only on internal/SQL
// failures; per-target outcomes (no-lock, not-owner, restore-failed) are
// reported via ReleaseResult.State.
func (s *Store) ReleaseLocks(ctx context.Context, targets []domain.Target, byAgent string) ([]ReleaseResult, error) {
	if len(targets) == 0 {
		return []ReleaseResult{}, nil
	}

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	owners, err := loadOwnersTx(ctx, tx, targets)
	if err != nil {
		return nil, err
	}

	results, owned := classifyReleases(targets, owners, byAgent)

	if len(owned) > 0 {
		if err := deleteOwnedTx(ctx, tx, owned, byAgent); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Chmod restore is outside the tx — locks ARE released. Failures surface
	// per-target AND batch into one audit event call (NORTH_STAR.md: every path
	// that removes a `locks` row also tries restore + audits failure).
	s.restoreAndAuditReleases(ctx, results, byAgent)
	return results, nil
}

// classifyReleases walks input targets in order, classifying each against the
// owners map and collecting the canonical paths to delete in one statement.
func classifyReleases(targets []domain.Target, owners map[string]string, byAgent string) ([]ReleaseResult, []string) {
	results := make([]ReleaseResult, len(targets))
	owned := make([]string, 0, len(targets))
	for i, t := range targets {
		results[i].Target = t
		o, ok := owners[t.Canonical]
		switch {
		case !ok:
			results[i].State = StateNoLock
		case o != byAgent:
			results[i].State = StateNotOwner
			results[i].Holder = o
		default:
			results[i].State = StateUnlocked
			owned = append(owned, t.Canonical)
		}
	}
	return results, owned
}

func (s *Store) restoreAndAuditReleases(ctx context.Context, results []ReleaseResult, byAgent string) {
	now := time.Now()
	var failEvents []domain.Event
	for i := range results {
		if results[i].State != StateUnlocked {
			continue
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			results[i].State = StateRestoreFailed
			results[i].RestoreErr = rerr
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
		}
	}
	if len(failEvents) > 0 {
		_ = s.AppendEvents(ctx, failEvents)
	}
}

// loadOwnersTx reads owner_uuid for the given targets via a single SELECT.
// Returned map is keyed by target_canonical; missing keys = no row.
func loadOwnersTx(ctx context.Context, tx *sql.Tx, targets []domain.Target) (map[string]string, error) {
	placeholders, args := inClause(targets)
	// placeholders is built from '?' chars only; user data flows via args.
	rows, err := tx.QueryContext(ctx, `SELECT target_canonical, owner_uuid FROM locks WHERE target_canonical IN (`+placeholders+`)`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, len(targets))
	for rows.Next() {
		var canonical, owner string
		if err := rows.Scan(&canonical, &owner); err != nil {
			return nil, err
		}
		out[canonical] = owner
	}
	return out, rows.Err()
}

// deleteOwnedTx removes `locks` rows for the given canonical paths owned by
// byAgent in one statement.
func deleteOwnedTx(ctx context.Context, tx *sql.Tx, canonicals []string, byAgent string) error {
	placeholders, args := inClauseStrings(canonicals)
	args = append(args, byAgent)
	_, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical IN (`+placeholders+`) AND owner_uuid = ?`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	return err
}

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

// restoreAndAudit re-adds owner-write to a released target and emits a
// mode_restore_failed event on failure. Spec contract (NORTH_STAR.md): strip
// on acquire, restore on release. Callers: BreakLock, reclaimStaleTx,
// DoctorRepair — every path that removes a `locks` row. ReleaseLocks inlines
// the equivalent so it can also report per-target StateRestoreFailed.
func (s *Store) restoreAndAudit(ctx context.Context, path, byAgent string) {
	if err := restoreWrite(path); err != nil {
		_ = s.appendModeRestoreFailedEvent(ctx, path, byAgent, time.Now(), err)
	}
}

// BreakResult is the per-target outcome from BreakLocks. Err is nil on success;
// ErrNoLockAtTarget or an AuthorizeBreak error otherwise.
type BreakResult struct {
	Target domain.Target
	Err    error
}

// BreakLock is a thin wrapper around BreakLocks for single-target callers.
func (s *Store) BreakLock(ctx context.Context, t domain.Target, byAgent string, force bool, reason string, live domain.PidLiveProbe) error {
	res, err := s.BreakLocks(ctx, []domain.Target{t}, byAgent, force, reason, live)
	if err != nil {
		return err
	}
	return res[0].Err
}

// BreakLocks force/stale-reclaims a batch of locks in one transaction. Per-target
// errors do not abort the batch — see BreakResult.Err. Returned error is non-nil
// only on internal/SQL failures. Results are returned in input order.
func (s *Store) BreakLocks(ctx context.Context, targets []domain.Target, byAgent string, force bool, reason string, live domain.PidLiveProbe) ([]BreakResult, error) {
	if len(targets) == 0 {
		return []BreakResult{}, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := loadLocksByTargetTx(ctx, tx, targets)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	kind := EventLockBroken
	if !force {
		kind = EventLockReclaimedStale
	}

	results, events, deleteByOwner := classifyBreaks(targets, existing, byAgent, force, kind, reason, now, live)

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
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.restoreAndAuditBreaks(ctx, results, byAgent, now)
	return results, nil
}

// classifyBreaks walks input targets in order, building per-target results, the
// batched event slice, and a per-owner canonical-path grouping for DELETE.
// Returning all three lets the caller emit one events insert and one DELETE per
// owner inside the same tx.
func classifyBreaks(
	targets []domain.Target,
	existing map[string]domain.LockRecord,
	byAgent string,
	force bool,
	kind string,
	reason string,
	now time.Time,
	live domain.PidLiveProbe,
) (results []BreakResult, events []domain.Event, deleteByOwner map[string][]string) {
	results = make([]BreakResult, len(targets))
	deleteByOwner = map[string][]string{}
	for i, t := range targets {
		results[i].Target = t
		l, ok := existing[t.Canonical]
		if !ok {
			results[i].Err = ErrNoLockAtTarget
			continue
		}
		if err := domain.AuthorizeBreak(l, force, now, l.Host, live); err != nil {
			results[i].Err = err
			continue
		}
		events = append(events, domain.Event{
			Target:      t,
			Kind:        kind,
			ActorUUID:   byAgent,
			SubjectUUID: l.OwnerUUID,
			Reason:      reason,
			CreatedAt:   now,
		})
		deleteByOwner[l.OwnerUUID] = append(deleteByOwner[l.OwnerUUID], t.Canonical)
	}
	return results, events, deleteByOwner
}

func (s *Store) restoreAndAuditBreaks(ctx context.Context, results []BreakResult, byAgent string, now time.Time) {
	var failEvents []domain.Event
	for i := range results {
		if results[i].Err != nil {
			continue
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			failEvents = append(failEvents, modeRestoreFailedEvent(results[i].Target.Canonical, byAgent, now, rerr))
		}
	}
	if len(failEvents) > 0 {
		_ = s.AppendEvents(ctx, failEvents)
	}
}

func loadLocksByTargetTx(ctx context.Context, tx *sql.Tx, targets []domain.Target) (map[string]domain.LockRecord, error) {
	placeholders, args := inClause(targets)
	rows, err := tx.QueryContext(ctx, `SELECT `+lockCols+` FROM locks WHERE target_canonical IN (`+placeholders+`)`, args...) //nolint:gosec // G202 placeholders are '?' chars only, all data via args

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]domain.LockRecord, len(targets))
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out[l.Target.Canonical] = l
	}
	return out, rows.Err()
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

const lockCols = `target_canonical,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,branch`

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
