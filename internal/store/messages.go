package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

func (s *Store) AddMessage(ctx context.Context, msg domain.Message) error {
	if msg.ID == "" {
		msg.ID = newTagID(msg.FromUUID, msg.CreatedAt, msg.Body)
		msg.ID = "m-" + msg.ID[2:] // same hash, different prefix
	}
	var expiresNs sql.NullInt64
	if msg.ExpiresAt != nil {
		expiresNs = sql.NullInt64{Valid: true, Int64: msg.ExpiresAt.UnixNano()}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(id,from_uuid,to_uuid,body,created_at,expires_at) VALUES (?,?,?,?,?,?)`,
		msg.ID, msg.FromUUID, msg.ToUUID, msg.Body, msg.CreatedAt.UnixNano(), expiresNs)
	return err
}

func (s *Store) ListUnreadMessages(ctx context.Context, toUUID string) ([]domain.Message, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
SELECT id,from_uuid,to_uuid,body,created_at,expires_at
FROM messages
WHERE to_uuid = ? AND read_at IS NULL
  AND (expires_at IS NULL OR expires_at > ?)
ORDER BY created_at, id`, toUUID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) MarkMessagesRead(ctx context.Context, toUUID string) error {
	readNs := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET read_at = ? WHERE to_uuid = ? AND read_at IS NULL`,
		readNs, toUUID)
	return err
}

// UnreadMessageSummary returns count and deduplicated sender UUIDs for the banner.
func (s *Store) UnreadMessageSummary(ctx context.Context, toUUID string) (int, []string, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
SELECT from_uuid, COUNT(*) FROM messages
WHERE to_uuid = ? AND read_at IS NULL
  AND (expires_at IS NULL OR expires_at > ?)
GROUP BY from_uuid
ORDER BY MIN(created_at)`, toUUID, now)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var total int
	var senders []string
	for rows.Next() {
		var from string
		var n int
		if err := rows.Scan(&from, &n); err != nil {
			return 0, nil, err
		}
		total += n
		senders = append(senders, from)
	}
	return total, senders, rows.Err()
}

func scanMessages(rows *sql.Rows) ([]domain.Message, error) {
	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		var createdNs int64
		var expiresNs sql.NullInt64
		if err := rows.Scan(&m.ID, &m.FromUUID, &m.ToUUID, &m.Body, &createdNs, &expiresNs); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(0, createdNs)
		if expiresNs.Valid {
			t := time.Unix(0, expiresNs.Int64)
			m.ExpiresAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
