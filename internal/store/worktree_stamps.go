package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"loto/internal/domain"
)

// RepairedWorktreeStamp is one lock or claim row RepairWorktreeStamps
// rewrote — or, for a lock that collided with a row already stamped for
// callerWorktree, dropped in favor of that existing row (Rules: "keeps one
// row, not two").
type RepairedWorktreeStamp struct {
	Kind      string // repairKindLock or repairKindClaim
	Canonical string // lock target_canonical, or claim path_prefix
	OldPath   string
}

// repairKindLock and repairKindClaim name RepairedWorktreeStamp.Kind. Named
// constants rather than inline literals so the string doesn't also need
// repeating at both append sites below (goconst).
const (
	repairKindLock  = "lock"
	repairKindClaim = "claim"
)

// RepairWorktreeStamps is loto-in0v's explicit `loto doctor --repair` fix for
// a moved or renamed worktree (Rules, dk 2026-09-26: option 2, an explicit
// doctor step — NOT an automatic migration at runtime open). Unlike
// DoctorRepair, this is never reached from internal/cli/runtime.go's open
// path; a caller must run `loto doctor --repair` on purpose.
//
// A stamp is rewritten to callerWorktree only when it provably IS this
// checkout:
//   - the row's owner_uuid matches agent (the caller), AND
//   - its worktree column names a path that no longer exists on disk
//     (worktreePathGone) — a live sibling worktree's path still stands, so
//     its rows are never touched, AND
//   - when movedFrom is non-empty, the stamped path matches it exactly (the
//     caller naming the one prior location explicitly via
//     `--moved-from <old>`, rather than this pass sweeping every stale path
//     this owner has ever left a row under).
//
// callerWorktree == "" (repo top could not be resolved) does no work: there
// is no "this checkout" to rewrite toward.
//
// A rewrite that would collide with a lock row already stamped
// (target_canonical, owner_uuid, callerWorktree) — this owner re-acquired the
// same path from the new location before running --repair — keeps that
// existing row and deletes the stale one, rather than producing two rows for
// one owner+target (Rules). Claims have no such case: their PRIMARY KEY is
// (path_prefix, owner_uuid) with no worktree component (schema.sql), so at
// most one row can ever exist per owner+prefix and the UPDATE below can never
// collide.
func (s *Store) RepairWorktreeStamps(ctx context.Context, agent domain.AgentUUID, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	if callerWorktree == "" {
		return nil, nil
	}
	byAgent := string(agent)

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	repaired, err := repairWorktreeStampsTx(ctx, tx, byAgent, callerWorktree, movedFrom)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	flock.release()
	return repaired, nil
}

// repairWorktreeStampsTx does the actual read-then-rewrite for both tables,
// inside the caller's tx. Split into one helper per table (below) to keep
// this at a glance: read candidates, rewrite each, merge and sort.
func repairWorktreeStampsTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	lockRepaired, err := repairStaleLocksTx(ctx, tx, byAgent, callerWorktree, movedFrom)
	if err != nil {
		return nil, err
	}
	claimRepaired, err := repairStaleClaimsTx(ctx, tx, byAgent, callerWorktree, movedFrom)
	if err != nil {
		return nil, err
	}
	out := make([]RepairedWorktreeStamp, 0, len(lockRepaired)+len(claimRepaired))
	out = append(out, lockRepaired...)
	out = append(out, claimRepaired...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Canonical < out[j].Canonical
	})
	return out, nil
}

// repairStaleLocksTx rewrites (or, on collision, drops) every lock row this
// owner holds whose worktree stamp names a path that no longer exists on
// disk. A collision — a row already stamped for callerWorktree at the same
// target — keeps that row and deletes the stale one (Rules: "keeps one row,
// not two").
func repairStaleLocksTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	rows, err := ownedStaleLockWorktreesTx(ctx, tx, byAgent, callerWorktree, movedFrom)
	if err != nil {
		return nil, err
	}
	var out []RepairedWorktreeStamp
	for _, r := range rows {
		if !worktreePathGone(r.oldPath) {
			continue // provably-live sibling worktree: never rewrite (Rules)
		}
		collided, err := lockRowExistsTx(ctx, tx, r.key, byAgent, callerWorktree)
		if err != nil {
			return nil, err
		}
		if collided {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ? AND worktree = ?`,
				r.key, byAgent, r.oldPath); err != nil {
				return nil, err
			}
		} else if _, err := tx.ExecContext(ctx,
			`UPDATE locks SET worktree = ? WHERE target_canonical = ? AND owner_uuid = ? AND worktree = ?`,
			callerWorktree, r.key, byAgent, r.oldPath); err != nil {
			return nil, err
		}
		out = append(out, RepairedWorktreeStamp{Kind: repairKindLock, Canonical: r.key, OldPath: r.oldPath})
	}
	return out, nil
}

// repairStaleClaimsTx rewrites every claim row this owner holds whose
// worktree stamp names a path that no longer exists on disk. No collision
// case: claims key on (path_prefix, owner_uuid) alone (schema.sql), so at
// most one row ever exists per owner+prefix — the very row being updated.
func repairStaleClaimsTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	rows, err := ownedStaleClaimWorktreesTx(ctx, tx, byAgent, callerWorktree, movedFrom)
	if err != nil {
		return nil, err
	}
	var out []RepairedWorktreeStamp
	for _, r := range rows {
		if !worktreePathGone(r.oldPath) {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE claims SET worktree = ? WHERE path_prefix = ? AND owner_uuid = ?`,
			callerWorktree, r.key, byAgent); err != nil {
			return nil, err
		}
		out = append(out, RepairedWorktreeStamp{Kind: repairKindClaim, Canonical: r.key, OldPath: r.oldPath})
	}
	return out, nil
}

// staleWorktreeRow pairs one owned row's identifying key (lock target or
// claim prefix) with the worktree path it is still stamped with.
type staleWorktreeRow struct {
	key     string
	oldPath string
}

func ownedStaleLockWorktreesTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]staleWorktreeRow, error) {
	q := `SELECT target_canonical, worktree FROM locks WHERE owner_uuid = ? AND worktree != '' AND worktree != ?`
	args := []any{byAgent, callerWorktree}
	if movedFrom != "" {
		q += ` AND worktree = ?`
		args = append(args, movedFrom)
	}
	return queryStaleWorktreeRows(ctx, tx, q+` ORDER BY target_canonical`, args)
}

func ownedStaleClaimWorktreesTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]staleWorktreeRow, error) {
	q := `SELECT path_prefix, worktree FROM claims WHERE owner_uuid = ? AND worktree != '' AND worktree != ?`
	args := []any{byAgent, callerWorktree}
	if movedFrom != "" {
		q += ` AND worktree = ?`
		args = append(args, movedFrom)
	}
	return queryStaleWorktreeRows(ctx, tx, q+` ORDER BY path_prefix`, args)
}

func queryStaleWorktreeRows(ctx context.Context, tx *sql.Tx, q string, args []any) ([]staleWorktreeRow, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []staleWorktreeRow
	for rows.Next() {
		var r staleWorktreeRow
		if err := rows.Scan(&r.key, &r.oldPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func lockRowExistsTx(ctx context.Context, tx *sql.Tx, target, byAgent, worktree string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM locks WHERE target_canonical = ? AND owner_uuid = ? AND worktree = ?`,
		target, byAgent, worktree).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
