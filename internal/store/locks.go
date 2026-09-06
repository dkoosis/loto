package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"loto/internal/domain"
)

const (
	EventLockAcquired       = "lock_acquired"
	EventLockReleased       = "lock_released"
	EventLockBroken         = "lock_broken"
	EventLockReclaimedStale = "lock_reclaimed_stale"
	// EventModeRestoreFailed is emitted only by doctor's chmod-era migration
	// now (loto-zssw); EventAcquireRollbackStart is emitted by nothing at all.
	// Both stay declared: the schema's event_kind CHECK still names them, and
	// rows written before the strip retired are still readable.
	EventModeRestoreFailed    = "mode_restore_failed"
	EventAcquireRollbackStart = "acquire_rollback_started"
	EventLockDowngraded       = "lock_downgraded"
	EventLockRefreshed        = "lock_refreshed"
	// EventGateBypass is emitted every time LOTO_GATE=off bypasses admission
	// (loto-ovno.4, git-gate.md "The gate can never become the outage"). No
	// Target — a bypass is a session-scoped fact, not a per-path one — so
	// TargetCanonical is written empty. ActorUUID names who bypassed.
	EventGateBypass = "gate_bypass"
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

// CandidateClaimConflictError reports the durable candidate claims blocking a
// lease acquisition (loto-ovno.2 part 3) — a distinct type from
// MultiConflictError rather than a lossy reuse, since a CandidateClaim is not
// a LockRecord: it names a candidate awaiting promotion, not a peer holding a
// lock, and a caller needs to say something different about each.
type CandidateClaimConflictError struct {
	Blockers []domain.CandidateClaim
}

func (e *CandidateClaimConflictError) Error() string {
	return fmt.Sprintf("candidate claim conflict: %d blocker(s)", len(e.Blockers))
}

func (e *MultiConflictError) Error() string {
	return fmt.Sprintf("multi-target lock conflict: %d blocker(s)", len(e.Blockers))
}

// ReleaseOutcome distinguishes the per-target result of a multi-target release.
type ReleaseOutcome int

const (
	// StateUnlocked: row deleted.
	StateUnlocked ReleaseOutcome = iota
	// StateNoLock: no row at target — caller wasn't holding it.
	StateNoLock
	// StateNotOwner: row exists but owned by another agent.
	StateNotOwner
	// StateReclaimedStale: caller held no row, and EVERY foreign holder was
	// stale — all reclaimed (rows deleted, lock_reclaimed_stale audited per
	// holder). One live foreign holder vetoes the whole target back to
	// StateNotOwner (loto-ebkc).
	StateReclaimedStale
)

// ReleaseResult is the per-target outcome from ReleaseLocks.
type ReleaseResult struct {
	Target domain.Target
	State  ReleaseOutcome
	// Owner is the vetoing live holder (StateNotOwner) or the first reclaimed
	// dead holder in created_at,owner order (StateReclaimedStale).
	Owner string
	Mode  string // mode of the released row; "" → exclusive (loto-k5el.2)
}

// BreakResult is the per-target outcome from BreakLocks. Err is nil on success;
// ErrNoLockAtTarget or an AuthorizeBreak error otherwise.
type BreakResult struct {
	Target domain.Target
	Err    error
	Mode   string // mode of the broken row; "" → exclusive (mirrors ReleaseResult.Mode)
}

const lockCols = `target_canonical,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,proc_start,branch,mode,beacon,epoch`

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

// keyMatch renders both halves of a target-key comparison — the SQL expression
// applied to the column and the bind value that pairs with it — so a lookup
// can be byte-exact or case-folded without either half drifting from the other
// (loto-8soe).
//
// ‡ Why SQL needs this and not just the in-memory predicates: the CLI mints
// every key folded on a case-folding filesystem, so live binaries agree. A row
// an OLDER loto wrote carries the on-disk spelling (`Foo.go`), and
// domain.EvalContext.SameTarget keeps that row BLOCKING against the folded key
// — but a byte-exact `target_canonical = ?` stops FINDING it, so it could
// never be released by name: `loto unlock foo.go` printed state=no-lock and
// exit 0 while the row went on refusing every peer until its TTL (review of
// #310).
//
// ‡ Only a lookup that finds a row BY THE NAME A CALLER TYPED uses this. A
// query whose key came out of a row it had already read — reclaimStaleTx, the
// tag delete keyed on its own host lock — stays byte-exact: both sides are the
// same stored string, and folding there would widen a self-join onto a
// different row.
//
// ‡ SQLite's LOWER is ASCII-only while strings.ToLower is Unicode-aware, so a
// legacy row whose path carries a non-ASCII capital may not be found by the
// folded name. That miss is fail-safe in the direction that matters — the row
// keeps blocking, it is the release that stalls — and no repo path in play
// here is non-ASCII.
type keyMatch struct{ fold bool }

// keys reports how this store compares target keys. Zero value (no
// WithCaseFoldedKeys) is byte-exact, which is both the pre-existing behavior
// and the permanently correct answer on a case-sensitive filesystem.
func (s *Store) keys() keyMatch { return keyMatch{fold: s.caseFold} }

// keysFor reads the same answer back off an EvalContext, for a helper that
// already carries one and has no Store handle. The store fills both from one
// field, so the two cannot disagree.
func keysFor(ec domain.EvalContext) keyMatch { return keyMatch{fold: ec.CaseFold} }

// col wraps a key column so it compares under the same rule key normalizes to.
func (k keyMatch) col(name string) string {
	if k.fold {
		return "LOWER(" + name + ")"
	}
	return name
}

// key normalizes one canonical — as a bind value, and as the map key a caller
// will index by the target it typed.
func (k keyMatch) key(canonical string) string {
	if k.fold {
		return strings.ToLower(canonical)
	}
	return canonical
}

// inTargets and inStrings are inClause/inClauseStrings with every arg
// normalized to match col's expression.
func (k keyMatch) inTargets(targets []domain.Target) (string, []any) {
	ph, args := inClause(targets)
	return ph, k.normalizeArgs(args)
}

func (k keyMatch) inStrings(ss []string) (string, []any) {
	ph, args := inClauseStrings(ss)
	return ph, k.normalizeArgs(args)
}

func (k keyMatch) normalizeArgs(args []any) []any {
	if !k.fold {
		return args
	}
	for i := range args {
		if s, ok := args[i].(string); ok {
			args[i] = strings.ToLower(s)
		}
	}
	return args
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
	return scanLocksRows(rows)
}

// scanLocksRows drains a lock-column result set into a slice, propagating the
// first scan or iteration error. Shared by the full-table and target-scoped
// queries so the row-accumulation loop lives in one place.
func scanLocksRows(rows *sql.Rows) ([]domain.LockRecord, error) {
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
	var owner string   // sqlite text column → domain.AgentUUID at the store boundary
	var session string // sqlite text column → domain.SessionUUID at the store boundary
	var createdNs, expiresNs int64
	// proc_start is nullable: legacy rows (added via in-place ALTER) hold NULL.
	// Map NULL → 0 (UNKNOWN) at the store boundary so domain logic never sees
	// the SQL-null distinction.
	var procStart sql.NullInt64
	// mode is NOT NULL DEFAULT 'exclusive' in fresh schema, but a NullString
	// keeps scan robust against any NULL legacy row; "" → EffectiveMode treats
	// it as exclusive.
	var mode sql.NullString
	// beacon is NOT NULL DEFAULT 0 in fresh schema and in the in-place ALTER,
	// so every row has a value; NullBool keeps the scan robust anyway and maps
	// an absent value to false — "not a beacon", the conservative reading,
	// since a false beacon makes guard refuse rather than move the tree.
	var beacon sql.NullBool
	if err := r.Scan(&canonical, &owner, &session, &l.Intent, &createdNs, &expiresNs, &l.Host, &l.PID, &procStart, &l.Branch, &mode, &beacon, &l.Epoch); err != nil {
		return l, err
	}
	l.Beacon = beacon.Valid && beacon.Bool
	l.OwnerUUID = domain.AgentUUID(owner)
	l.SessionUUID = domain.SessionUUID(session)
	l.Target = domain.Target{Canonical: canonical}
	l.CreatedAt = time.Unix(0, createdNs).UTC()
	l.ExpiresAt = time.Unix(0, expiresNs).UTC()
	if procStart.Valid {
		l.ProcStart = procStart.Int64
	}
	if mode.Valid {
		l.Mode = mode.String
	}
	return l, nil
}

func reclaimStaleTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord, byAgent string, now time.Time) error {
	if err := appendEventTx(ctx, tx, domain.Event{
		Target:      stale.Target,
		Kind:        EventLockReclaimedStale,
		ActorUUID:   byAgent,
		SubjectUUID: string(stale.OwnerUUID),
		Reason:      "reclaimed stale lock",
		CreatedAt:   now,
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, stale.Target.Canonical, string(stale.OwnerUUID)); err != nil {
		return err
	}
	return nil
}
