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
func (s *Store) LockForOwnerAt(ctx context.Context, t domain.Target, owner domain.AgentUUID) (*domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		t.Canonical, string(owner))
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

// LocksForOwnerAt is the batched LockForOwnerAt: one owner-scoped query over the
// whole target set, returning owner's lock at each target keyed by canonical
// path. A target the owner does not hold is absent from the map — the caller
// reads a missing entry exactly as LockForOwnerAt's (nil,nil) "no row". Under the
// composite PK (target_canonical, owner_uuid) each (target, owner) pair yields at
// most one row, so the map is unambiguous; this collapses a lane assert's 2N
// point queries to one (loto-89n3).
func (s *Store) LocksForOwnerAt(ctx context.Context, targets []domain.Target, owner domain.AgentUUID) (map[string]domain.LockRecord, error) {
	out := make(map[string]domain.LockRecord, len(targets))
	if len(targets) == 0 {
		return out, nil
	}
	placeholders, args := inClause(targets)
	args = append([]any{string(owner)}, args...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE owner_uuid = ? AND target_canonical IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out[l.Target.Canonical] = l
	}
	return out, rows.Err()
}

// LockAt returns the longest-standing holder of target, or (nil,nil) if none.
// Under the composite PK a shared target may have several holders; the ORDER BY
// makes the choice deterministic (oldest created_at, then owner_uuid) rather
// than the arbitrary row SQLite returned before (loto-2nc5). Production tag
// delivery now fans out to every holder via LocksAt; LockAt remains for callers
// that legitimately want a single representative holder.
func (s *Store) LockAt(ctx context.Context, t domain.Target) (*domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE target_canonical = ? ORDER BY created_at ASC, owner_uuid ASC LIMIT 1`,
		t.Canonical)
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

// LocksAt returns EVERY holder of target in deterministic order (oldest lock
// first), or an empty slice if none. Under the composite PK a shared target may
// carry several coexisting holders. Tag delivery uses this to leave the note on
// all current holders — a note "on this file" must reach every blocker, not an
// arbitrary one (loto-2nc5).
func (s *Store) LocksAt(ctx context.Context, t domain.Target) ([]domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE target_canonical = ? ORDER BY created_at ASC, owner_uuid ASC`,
		t.Canonical)
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
