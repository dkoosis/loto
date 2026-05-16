package store

import (
	"context"

	"loto/internal/domain"
)

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
