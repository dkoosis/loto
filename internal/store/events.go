package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

const eventCols = `id,target_canonical,event_kind,actor_uuid,subject_uuid,reason,created_at`

// Events retention: cap at 1000 rows AND 7 days; rows violating either rule
// are deleted. Rotation runs opportunistically on each AcquireLocks call
// (cheap, in-txn) and on demand via Store.RotateEvents (called by
// `loto doctor --repair`).
const (
	eventsRetentionMax = 1000
	eventsRetentionAge = 7 * 24 * time.Hour
)

// RotateEvents trims the events table per retention policy.
func (s *Store) RotateEvents(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := rotateEventsTx(ctx, tx, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func rotateEventsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	cutoffNs := now.Add(-eventsRetentionAge).UnixNano()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, cutoffNs); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
DELETE FROM events WHERE id IN (
  SELECT id FROM events ORDER BY created_at DESC, id DESC LIMIT -1 OFFSET ?
)`, eventsRetentionMax)
	return err
}

func (s *Store) AppendEvent(ctx context.Context, e domain.Event) (string, error) {
	if e.ID == "" {
		e.ID = newEventID(e.ActorUUID, e.CreatedAt, e.Reason)
	}
	var subject sql.NullString
	if e.SubjectUUID != "" {
		subject = sql.NullString{Valid: true, String: e.SubjectUUID}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO events(`+eventCols+`) VALUES (?,?,?,?,?,?,?)`,
		e.ID, e.Target.Canonical, e.Kind, e.ActorUUID, subject, e.Reason, e.CreatedAt.UnixNano())
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

func appendEventTx(ctx context.Context, tx *sql.Tx, e domain.Event) error {
	if e.ID == "" {
		e.ID = newEventID(e.ActorUUID, e.CreatedAt, e.Reason)
	}
	var subject sql.NullString
	if e.SubjectUUID != "" {
		subject = sql.NullString{Valid: true, String: e.SubjectUUID}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events(`+eventCols+`) VALUES (?,?,?,?,?,?,?)`,
		e.ID, e.Target.Canonical, e.Kind, e.ActorUUID, subject, e.Reason, e.CreatedAt.UnixNano())
	return err
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
