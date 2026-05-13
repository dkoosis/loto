package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

const eventCols = `id,target_canonical,event_kind,actor_uuid,subject_uuid,reason,created_at`

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
