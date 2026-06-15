package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"loto/internal/domain"
)

const eventCols = `id,target_canonical,event_kind,actor_uuid,subject_uuid,reason,created_at`

// Events retention: cap at 1000 rows AND 7 days; rows violating either rule
// are deleted. Rotation runs in-txn via rotateEventsTx — opportunistically on
// each AcquireLocks call (cheap) and inside the `loto doctor --repair` tx.
const (
	eventsRetentionMax = 1000
	eventsRetentionAge = 7 * 24 * time.Hour
)

// rotateEventsTx trims the events table per retention policy, in the caller's tx.
func rotateEventsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoffNs := now.Add(-eventsRetentionAge).UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, cutoffNs); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE id IN (
  SELECT id FROM events ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?
)`, eventsRetentionMax)
	return err
}

// auditDetachedTimeout bounds best-effort audit writes that must survive
// parent-ctx cancellation. The post-commit restore loop and the
// post-rollback failure-audit must persist their breadcrumbs even when
// the caller has gone away — otherwise a cancelled lock attempt that
// leaves files in orphan-mode produces zero audit (gh#122).
const auditDetachedTimeout = 2 * time.Second

// auditDetachedHook fires at the start of every appendAuditDetached call, after
// the empty-input short-circuit. Default no-op; tests replace it to observe or
// widen the detached-audit window — e.g. loto-3qev asserts the op-flock is
// already released by the time this write tx runs in doctor/break/release. Same
// test-seam pattern as vacuumFn/commitTxFn.
var auditDetachedHook = func() {}

// appendAuditDetached appends evs on a fresh background context bounded
// by auditDetachedTimeout so audit writes survive parent-ctx cancellation.
// On failure (tx contention, SQLITE_BUSY, disk-full), the error is logged to
// s.stderr so the loss is observable instead of silent — callers that need
// per-target propagation should also surface the returned err via AuditErr
// (gh#107). The audit trail is hygiene, not an invariant: failure here does
// not undo the operation that's already on disk.
func (s *Store) appendAuditDetached(evs []domain.Event) error {
	if len(evs) == 0 {
		return nil
	}
	auditDetachedHook()
	ctx, cancel := context.WithTimeout(context.Background(), auditDetachedTimeout)
	defer cancel()
	err := s.AppendEvents(ctx, evs)
	if err != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "loto: audit-write failed for %d event(s): %v\n", len(evs), err)
	}
	return err
}

// AppendEvent inserts a single event. Thin wrapper around AppendEvents to keep
// the 1-arg caller surface; new code should prefer AppendEvents.
func (s *Store) AppendEvent(ctx context.Context, e domain.Event) (string, error) {
	evs := []domain.Event{e}
	if err := s.AppendEvents(ctx, evs); err != nil {
		return "", err
	}
	return evs[0].ID, nil
}

// AppendEvents inserts a batch of events in a single transaction. Empty input
// is a no-op. Event.ID is assigned in-place when empty so callers can read it
// back after the call.
func (s *Store) AppendEvents(ctx context.Context, evs []domain.Event) error {
	if len(evs) == 0 {
		return nil
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := appendEventsTx(ctx, tx, evs); err != nil {
		return err
	}
	return tx.Commit()
}

func appendEventTx(ctx context.Context, tx *sql.Tx, e domain.Event) error {
	evs := []domain.Event{e}
	return appendEventsTx(ctx, tx, evs)
}

func appendEventsTx(ctx context.Context, tx *sql.Tx, evs []domain.Event) error {
	for i := range evs {
		if evs[i].ID == "" {
			evs[i].ID = newEventID()
		}
		var subject sql.NullString
		if evs[i].SubjectUUID != "" {
			subject = sql.NullString{Valid: true, String: evs[i].SubjectUUID}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(`+eventCols+`) VALUES (?,?,?,?,?,?,?)`,
			evs[i].ID, evs[i].Target.Canonical, evs[i].Kind, evs[i].ActorUUID, subject, evs[i].Reason, evs[i].CreatedAt.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EventsForTarget(ctx context.Context, t domain.Target) ([]domain.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventCols+` FROM events WHERE target_canonical = ? ORDER BY created_at, id`, t.Canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func (s *Store) ListEvents(ctx context.Context) ([]domain.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventCols+` FROM events ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]domain.Event, error) {
	var out []domain.Event
	for rows.Next() {
		var (
			e         domain.Event
			canonical string
			subject   sql.NullString
			createdNs int64
		)
		if err := rows.Scan(&e.ID, &canonical, &e.Kind, &e.ActorUUID, &subject, &e.Reason, &createdNs); err != nil {
			return nil, err
		}
		e.Target = domain.Target{Canonical: canonical}
		if subject.Valid {
			e.SubjectUUID = subject.String
		}
		e.CreatedAt = time.Unix(0, createdNs).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
