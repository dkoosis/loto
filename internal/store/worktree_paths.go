package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// worktreePathsDDL mirrors schema.sql's declaration. Fresh DBs get the table
// straight from schema.sql; this is the in-place upgrade path for a DB that
// predates it (ensureClaimsTable precedent).
const worktreePathsDDL = `
CREATE TABLE IF NOT EXISTS worktree_paths (
  worktree_id TEXT NOT NULL,
  host        TEXT NOT NULL,
  path        TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (worktree_id, host)
);`

// ensureWorktreePathsTable adds the worktree_paths table to a DB that
// predates it (loto-in0v). user_version intentionally NOT bumped
// (ensureClaimsTable precedent).
func ensureWorktreePathsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "worktree_paths", worktreePathsDDL)
}

// SyncWorktreePath keeps the worktree_paths registry pointed at THIS
// checkout's current absolute path for (worktreeID, host), and migrates every
// locks/claims row still stamped with a stale path when it finds one
// (loto-in0v): `git worktree move`, or a bare rename of the checkout
// directory, changes repoTop while LockRecord.Worktree / ClaimRecord.Worktree
// keep the OLD value, so unlock --all and conflict scoping
// (domain.SameWorktree) start reading this checkout's own rows as a
// stranger's. Rewriting the stamp — not what it means — is the fix: every
// existing comparison against the CURRENT repoTop already does the right
// thing once the stamp matches reality again.
//
// worktreeID is gate.WorktreeID's answer for this checkout: "" for the
// primary worktree (fine as a key here — this table is store-internal and
// never reaches domain.SameWorktree's own "" = unknown convention), otherwise
// git's own move-stable admin-dir name for a linked one.
//
// First sighting of a (worktreeID, host) pair just records currentPath —
// there is nothing to migrate away from. A pair already pointed at
// currentPath is a no-op. Runs in one transaction so a mid-migration crash
// cannot leave some rows rewritten and others not.
func (s *Store) SyncWorktreePath(ctx context.Context, worktreeID, host, currentPath string) error {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var oldPath string
	err = tx.QueryRowContext(ctx,
		`SELECT path FROM worktree_paths WHERE worktree_id = ? AND host = ?`,
		worktreeID, host).Scan(&oldPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO worktree_paths (worktree_id, host, path, updated_at) VALUES (?, ?, ?, ?)`,
			worktreeID, host, currentPath, time.Now().UnixNano()); err != nil {
			return err
		}
		return commitTxFn(tx)
	case err != nil:
		return err
	case oldPath == currentPath:
		return nil // steady state: this worktree has not moved
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET worktree = ? WHERE worktree = ?`, currentPath, oldPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE claims SET worktree = ? WHERE worktree = ?`, currentPath, oldPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE worktree_paths SET path = ?, updated_at = ? WHERE worktree_id = ? AND host = ?`,
		currentPath, time.Now().UnixNano(), worktreeID, host); err != nil {
		return err
	}
	return commitTxFn(tx)
}
