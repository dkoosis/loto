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
	return scanLocksRows(rows)
}

// LockForOwnerAt returns the single lock at target held by owner IN THIS
// STORE'S OWN WORKTREE, or (nil,nil) if none. Replaces LockAt for the
// multi-holder world: under the composite PK LockAt's bare WHERE
// target_canonical=? can match several rows (a shared target with several
// holders) and returns an arbitrary one (loto-k5el.2).
//
// ‡ Worktree-scoped (loto-8z87): since the PK widened to (target_canonical,
// owner_uuid, worktree), one owner can hold a row per linked worktree of this
// repo. A caller asking "do I hold this" means its OWN checkout — s.repoTop —
// not a sibling's; a legacy row (worktree "") still matches any repoTop
// (domain.SameWorktree). A store opened without a repo top (repoTop == "")
// keeps today's reach: every worktree's row is visible, same as before this
// bead.
func (s *Store) LockForOwnerAt(ctx context.Context, t domain.Target, owner domain.AgentUUID) (*domain.LockRecord, error) {
	k := s.keys()
	wtCond, wtArgs := worktreeFilter(s.repoTop)
	args := append([]any{k.key(t.Canonical), string(owner)}, wtArgs...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE `+k.col("target_canonical")+` = ? AND owner_uuid = ? AND `+wtCond, //nolint:gosec // G202 k.col renders a fixed column expression, all data via args
		args...)
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
// path. A target the owner does not hold in THIS STORE'S OWN WORKTREE
// (s.repoTop, or a legacy "" row — loto-8z87) is absent from the map — the
// caller reads a missing entry exactly as LockForOwnerAt's (nil,nil) "no row".
func (s *Store) LocksForOwnerAt(ctx context.Context, targets []domain.Target, owner domain.AgentUUID) (map[string]domain.LockRecord, error) {
	out := make(map[string]domain.LockRecord, len(targets))
	if len(targets) == 0 {
		return out, nil
	}
	k := s.keys()
	placeholders, args := k.inTargets(targets)
	wtCond, wtArgs := worktreeFilter(s.repoTop)
	args = append(append([]any{string(owner)}, wtArgs...), args...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE owner_uuid = ? AND `+wtCond+` AND `+k.col("target_canonical")+` IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
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
		// Keyed by the normalized canonical, not the stored one: the caller
		// indexes this map by the target it typed, and a legacy row's stored
		// spelling is exactly what would not match (loto-8soe).
		out[k.key(l.Target.Canonical)] = l
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
		`SELECT `+lockCols+` FROM locks WHERE `+s.keys().col("target_canonical")+` = ? ORDER BY created_at ASC, owner_uuid ASC LIMIT 1`, //nolint:gosec // G202 k.col renders a fixed column expression, all data via args
		s.keys().key(t.Canonical))
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
		`SELECT `+lockCols+` FROM locks WHERE `+s.keys().col("target_canonical")+` = ? ORDER BY created_at ASC, owner_uuid ASC`, //nolint:gosec // G202 k.col renders a fixed column expression, all data via args
		s.keys().key(t.Canonical))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLocksRows(rows)
}
