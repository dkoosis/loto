package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"loto/internal/domain"
)

func (s *Store) UnreadTagsForAddressee(ctx context.Context, agent string, t domain.Target) ([]domain.TagRecord, error) {
	var cursorNs int64
	err := s.db.QueryRowContext(ctx, `SELECT last_read_at FROM read_cursors WHERE agent_uuid = ? AND target_canonical = ?`, agent, t.Canonical).Scan(&cursorNs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
SELECT `+tagCols+` FROM tags
WHERE target_canonical = ? AND addressee_uuid = ?
  AND created_at > ?
  AND (expires_at IS NULL OR expires_at > ?)
ORDER BY created_at, id`,
		t.Canonical, agent, cursorNs, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

func (s *Store) MarkRead(ctx context.Context, agent string, t domain.Target) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var maxNs sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT MAX(created_at) FROM tags WHERE target_canonical = ? AND addressee_uuid = ?`, t.Canonical, agent).Scan(&maxNs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !maxNs.Valid {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO read_cursors(agent_uuid,target_canonical,last_read_at) VALUES (?,?,?)
ON CONFLICT(agent_uuid,target_canonical) DO UPDATE SET last_read_at = excluded.last_read_at`,
		agent, t.Canonical, maxNs.Int64)
	if err != nil {
		return err
	}
	return tx.Commit()
}
