package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"loto/internal/domain"
)

// claimsDDL is the claims table shape, duplicated from schema.sql per the
// ensureLocksModeAndPK precedent. In practice ensureClaimsTable's pending
// probe is what routes a stamped pre-claims DB back into migrate — whose
// schemaSQL pass creates the table before the ensures run — so the apply
// branch is a contract-conforming backstop (a pending ensureFn must be able
// to apply itself), not the usual delivery path. No user_version bump.
const claimsDDL = `
CREATE TABLE IF NOT EXISTS claims (
  path_prefix  TEXT NOT NULL,
  owner_uuid   TEXT NOT NULL,
  session_uuid TEXT NOT NULL DEFAULT '',
  intent       TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  host         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path_prefix, owner_uuid)
);
CREATE INDEX IF NOT EXISTS idx_claims_expires ON claims(expires_at);`

// ClaimConflictError reports the live claims whose territory overlaps the
// requested prefix. Blockers are sorted prefix then created_at.
type ClaimConflictError struct {
	Blockers []domain.ClaimRecord
}

func (e *ClaimConflictError) Error() string {
	return fmt.Sprintf("claim conflict: %d blocker(s)", len(e.Blockers))
}

// ClaimReleaseState distinguishes the outcome of ReleaseClaim.
type ClaimReleaseState int

const (
	// ClaimStateReleased: the caller's row at the prefix was deleted.
	ClaimStateReleased ClaimReleaseState = iota
	// ClaimStateNoClaim: no row at the prefix — nothing was reserved.
	ClaimStateNoClaim
	// ClaimStateNotOwner: row(s) exist at the prefix but none is the caller's.
	ClaimStateNotOwner
)

// ClaimReleaseResult is the outcome of ReleaseClaim. Owner names the actual
// holder when State == ClaimStateNotOwner (deterministic pick: oldest
// created_at, then owner_uuid).
type ClaimReleaseResult struct {
	PathPrefix string
	State      ClaimReleaseState
	Owner      string
}

// ClaimPrefix atomically reserves rec.PathPrefix as territory, mirroring the
// AcquireLocks skeleton (locks_acquire.go): acquireOpFlock → beginTx
// (immediate write txn) → in-tx read → partition {same-owner refresh,
// expired-reclaim, live blockers} via the domain overlap predicate → conflict
// (rollback) or delete-expired + upsert → commit. Both racers serialize on
// the flock; the loser's in-tx read sees the winner's committed row. The
// in-tx predicate — NOT the (path_prefix, owner_uuid) PK, which would happily
// admit cross-owner duplicates — is the real guard (loto-7af9). Claims strip
// no write bits, so there is no chmod half and no claim events in v1.
func (s *Store) ClaimPrefix(ctx context.Context, rec domain.ClaimRecord) error {
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	all, err := loadClaimsTx(ctx, tx)
	if err != nil {
		return err
	}
	blockers, expired := partitionClaims(all, rec, time.Now())
	if len(blockers) > 0 {
		return &ClaimConflictError{Blockers: blockers}
	}
	// Lazy-reclaim: expired overlapping rows die inside the winner's txn — the
	// reclaim shape of reclaimStaleAndCollectBlockers minus the chmod half.
	for i := range expired {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM claims WHERE path_prefix = ? AND owner_uuid = ?`,
			expired[i].PathPrefix, string(expired[i].OwnerUUID)); err != nil {
			return err
		}
	}
	if err := insertOrRefreshClaim(ctx, tx, rec); err != nil {
		return err
	}
	return commitTxFn(tx)
}

// partitionClaims splits the rows overlapping rec.PathPrefix into live
// blockers and expired-reclaim candidates. Same-owner rows never land in
// either bucket — a same-owner overlap never blocks, and the exact row
// refreshes via upsert. Blockers come back sorted prefix then created_at.
func partitionClaims(all []domain.ClaimRecord, rec domain.ClaimRecord, now time.Time) (blockers, expired []domain.ClaimRecord) {
	for i := range all {
		ex := &all[i]
		if !domain.PrefixOverlaps(ex.PathPrefix, rec.PathPrefix) || ex.OwnerUUID == rec.OwnerUUID {
			continue
		}
		if ex.Expired(now) {
			expired = append(expired, *ex)
			continue
		}
		blockers = append(blockers, *ex)
	}
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].PathPrefix != blockers[j].PathPrefix {
			return blockers[i].PathPrefix < blockers[j].PathPrefix
		}
		return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
	})
	return blockers, expired
}

// insertOrRefreshClaim upserts the caller's claim row. ON CONFLICT targets the
// (path_prefix, owner_uuid) PK so a same-owner re-claim refreshes TTL/intent
// in place; created_at is deliberately NOT updated — the original reservation
// time survives a refresh (mirrors insertOrRefreshLock).
func insertOrRefreshClaim(ctx context.Context, tx *sql.Tx, c domain.ClaimRecord) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO claims(path_prefix, owner_uuid, session_uuid, intent, created_at, expires_at, host)
VALUES (?,?,?,?,?,?,?)
ON CONFLICT(path_prefix, owner_uuid) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host`,
		c.PathPrefix, string(c.OwnerUUID), string(c.SessionUUID),
		c.Intent, c.CreatedAt.UnixNano(), c.ExpiresAt.UnixNano(), c.Host,
	)
	return err
}

// ReleaseClaim deletes the caller's claim at exactly prefix. Outcomes:
// Released (row deleted), NoClaim (no live row at prefix), NotOwner (a LIVE
// foreign row exists — Owner names the actual holder). The not-owner probe
// filters expired rows, matching partitionClaims/status staleness semantics:
// a dead lease that accreted at the prefix must not block the verdict. One
// immediate write txn keeps the delete and the outcome probe consistent.
func (s *Store) ReleaseClaim(ctx context.Context, prefix string, owner domain.AgentUUID) (ClaimReleaseResult, error) {
	out := ClaimReleaseResult{PathPrefix: prefix}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return out, err
	}
	defer cleanup()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM claims WHERE path_prefix = ? AND owner_uuid = ?`,
		prefix, string(owner))
	if err != nil {
		return out, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return out, err
	}
	if n > 0 {
		out.State = ClaimStateReleased
		return out, commitTxFn(tx)
	}
	var holder string
	err = tx.QueryRowContext(ctx,
		`SELECT owner_uuid FROM claims WHERE path_prefix = ? AND expires_at > ? ORDER BY created_at ASC, owner_uuid ASC LIMIT 1`,
		prefix, time.Now().UnixNano()).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.State = ClaimStateNoClaim
		return out, nil
	case err != nil:
		return out, err
	}
	out.State = ClaimStateNotOwner
	out.Owner = holder
	return out, nil
}

const claimCols = `path_prefix,owner_uuid,session_uuid,intent,created_at,expires_at,host`

// ListClaims returns every claim row, expired included — staleness is a
// display-time judgment (status filters via Expired), mirroring ListLocks.
func (s *Store) ListClaims(ctx context.Context) ([]domain.ClaimRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+claimCols+` FROM claims`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClaimsRows(rows)
}

func loadClaimsTx(ctx context.Context, tx *sql.Tx) ([]domain.ClaimRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+claimCols+` FROM claims`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClaimsRows(rows)
}

func scanClaimsRows(rows *sql.Rows) ([]domain.ClaimRecord, error) {
	var out []domain.ClaimRecord
	for rows.Next() {
		var c domain.ClaimRecord
		var owner, session string
		var createdNs, expiresNs int64
		if err := rows.Scan(&c.PathPrefix, &owner, &session, &c.Intent, &createdNs, &expiresNs, &c.Host); err != nil {
			return nil, err
		}
		c.OwnerUUID = domain.AgentUUID(owner)
		c.SessionUUID = domain.SessionUUID(session)
		c.CreatedAt = time.Unix(0, createdNs).UTC()
		c.ExpiresAt = time.Unix(0, expiresNs).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// ensureClaimsTable adds the claims table to a stamped DB that predates it
// (loto-7af9). Pending when the table is absent; not-pending on a fresh DB
// (schemaSQL already declared it) and on every re-Open. The probe doubles as
// the claims-table existence check for schemaCurrent (ErrNoRows → pending).
// user_version intentionally NOT bumped (loto-kwlp precedent).
func ensureClaimsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='claims'`).Scan(&name)
	if err == nil {
		return false, nil // already present
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if apply {
		if _, err := db.ExecContext(ctx, claimsDDL); err != nil {
			return false, err
		}
		return false, nil // applied: no longer outstanding
	}
	return true, nil
}
