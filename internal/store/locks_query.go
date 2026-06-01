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

// LockForOwnerAt returns the single lock at target held by owner, or (nil,nil)
// if none. Replaces LockAt for the multi-holder world: under the composite PK
// LockAt's bare WHERE target_canonical=? can match several rows (a shared
// target with several holders) and returns an arbitrary one (loto-k5el.2).
func (s *Store) LockForOwnerAt(ctx context.Context, t domain.Target, owner string) (*domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		t.Canonical, owner)
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
	return &l, rows.Err()
}

// LockAt returns one lock at target, or (nil,nil) if none. Under the composite
// PK a shared target may have several holders; LockAt returns an ARBITRARY one.
// Callers needing a specific holder must use LockForOwnerAt; callers needing all
// holders must filter ListLocks. The sole remaining caller (tag delivery) only
// needs "is anyone holding this, and who can I attach the tag to" — any holder
// is acceptable there (loto-k5el.2 T5.5).
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
