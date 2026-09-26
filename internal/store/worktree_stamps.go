package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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

// RepairWorktreeStamps is loto-in0v's explicit `loto doctor --repair
// --moved-from <old>` fix for a moved or renamed worktree (Rules, dk
// 2026-09-26: an explicit doctor step — NOT an automatic migration at runtime
// open). It is never reached from internal/cli/runtime.go's open path.
//
// A stamp is rewritten to callerWorktree only when it provably IS this
// checkout:
//   - the caller named the old path explicitly (movedFrom), AND
//   - the row's owner_uuid matches agent (the caller), AND
//   - the stamped path equals movedFrom and no longer exists on disk
//     (worktreePathGone) — a live sibling worktree's path still stands, so
//     its rows are never touched.
//
// There is no implicit mode. "Owner matches and the path is gone" does not
// tell a MOVED checkout from a DELETED sibling: one pinned LOTO_AGENT_ID may
// own rows in two worktrees (loto-8z87), and adopting a removed sibling's
// locks would hand this checkout locks it never took (PR #376 review, Codex
// P1). movedFrom == "" therefore rewrites nothing; `loto doctor` reports the
// stale path with the exact --moved-from command instead.
//
// callerWorktree == "" (repo top could not be resolved) does no work: there
// is no "this checkout" to rewrite toward. movedFrom must be absolute; it is
// compared after filepath.Clean, the form stamps are written in.
//
// A rewrite that would collide with a lock row already stamped
// (target_canonical, owner_uuid, callerWorktree) — this owner re-acquired the
// same path from the new location before running --repair — keeps that
// existing row and deletes the stale one, re-homing the stale row's tags onto
// the survivor so a pending note is not orphaned (Rules: "keeps one row, not
// two"). Claims have no such case: their PRIMARY KEY is (path_prefix,
// owner_uuid) with no worktree component (schema.sql), so at most one row can
// ever exist per owner+prefix and the UPDATE below can never collide.
func (s *Store) RepairWorktreeStamps(ctx context.Context, agent domain.AgentUUID, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	return s.runWorktreeStampRepair(ctx, agent, callerWorktree, movedFrom, true)
}

// PreviewWorktreeStampRepair reports exactly the rows RepairWorktreeStamps
// would rewrite or collapse for the same arguments, writing nothing: the same
// tx runs and is rolled back (`loto doctor --dry-run`).
func (s *Store) PreviewWorktreeStampRepair(ctx context.Context, agent domain.AgentUUID, callerWorktree, movedFrom string) ([]RepairedWorktreeStamp, error) {
	return s.runWorktreeStampRepair(ctx, agent, callerWorktree, movedFrom, false)
}

// ErrMovedFromNotAbsolute is returned when --moved-from names a relative
// path: stamps are absolute checkout roots, so a relative one can never match.
var ErrMovedFromNotAbsolute = errors.New("loto: --moved-from must be an absolute path")

func (s *Store) runWorktreeStampRepair(ctx context.Context, agent domain.AgentUUID, callerWorktree, movedFrom string, commit bool) ([]RepairedWorktreeStamp, error) {
	if callerWorktree == "" || movedFrom == "" {
		return nil, nil
	}
	if !filepath.IsAbs(movedFrom) {
		return nil, ErrMovedFromNotAbsolute
	}
	movedFrom = filepath.Clean(movedFrom)
	if movedFrom == callerWorktree {
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
	if !commit {
		return repaired, nil // cleanup rolls the tx back
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
			if err := rehomeCollidedTagsTx(ctx, tx, r.key, byAgent, r.oldPath, callerWorktree); err != nil {
				return nil, err
			}
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
	return queryStaleWorktreeRows(ctx, tx,
		`SELECT target_canonical, worktree FROM locks WHERE owner_uuid = ? AND worktree = ? AND worktree != ? ORDER BY target_canonical`,
		[]any{byAgent, movedFrom, callerWorktree})
}

func ownedStaleClaimWorktreesTx(ctx context.Context, tx *sql.Tx, byAgent, callerWorktree, movedFrom string) ([]staleWorktreeRow, error) {
	return queryStaleWorktreeRows(ctx, tx,
		`SELECT path_prefix, worktree FROM claims WHERE owner_uuid = ? AND worktree = ? AND worktree != ? ORDER BY path_prefix`,
		[]any{byAgent, movedFrom, callerWorktree})
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

// rehomeCollidedTagsTx points every tag hosted by the stale lock row
// (target, owner, oldPath) at the surviving row stamped callerWorktree. Tags
// key their host on (target_canonical, lock_owner_uuid, lock_created_at), and
// the survivor was usually acquired later, so without this the stale row's
// DELETE would leave its pending notes joined to nothing — invisible to the
// deferred tag footer and swept by the next repair GC (PR #376 review, Codex
// P2). The alive-tag cap is an insert-time guard and is not re-applied here:
// dropping a note to honor it would lose exactly what this preserves.
func rehomeCollidedTagsTx(ctx context.Context, tx *sql.Tx, target, byAgent, oldPath, callerWorktree string) error {
	var staleAt, keepAt int64
	if err := tx.QueryRowContext(ctx,
		`SELECT created_at FROM locks WHERE target_canonical = ? AND owner_uuid = ? AND worktree = ?`,
		target, byAgent, oldPath).Scan(&staleAt); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT created_at FROM locks WHERE target_canonical = ? AND owner_uuid = ? AND worktree = ?`,
		target, byAgent, callerWorktree).Scan(&keepAt); err != nil {
		return err
	}
	if staleAt == keepAt {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE tags SET lock_created_at = ? WHERE target_canonical = ? AND lock_owner_uuid = ? AND lock_created_at = ?`,
		keepAt, target, byAgent, staleAt)
	return err
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
