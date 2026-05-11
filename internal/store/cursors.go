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

// MarkRead advances the read cursor for (agent, target) to upTo. Callers
// must pass the timestamp of the latest tag actually displayed, not query
// MAX(created_at) themselves — see gh#47. The previous implementation
// re-read MAX inside the write tx, which advanced the cursor past tags
// inserted between display and MarkRead. The cursor never moves backward:
// a stale call with an older upTo cannot regress a newer cursor.
func (s *Store) MarkRead(ctx context.Context, agent string, t domain.Target, upTo time.Time) error {
	if upTo.IsZero() {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO read_cursors(agent_uuid,target_canonical,last_read_at) VALUES (?,?,?)
ON CONFLICT(agent_uuid,target_canonical) DO UPDATE SET last_read_at = MAX(read_cursors.last_read_at, excluded.last_read_at)`,
		agent, t.Canonical, upTo.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}
