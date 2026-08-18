package store

import (
	"context"
	"database/sql"

	"loto/internal/domain"
)

// pathEpochsDDL is the path_epochs table shape, duplicated from schema.sql per
// the claimsDDL precedent: a pending ensureFn must be able to apply itself,
// even though the usual delivery path is migrate's schemaSQL pass.
const pathEpochsDDL = `
CREATE TABLE IF NOT EXISTS path_epochs (
  path_canonical TEXT PRIMARY KEY,
  epoch          INTEGER NOT NULL
);`

// nextPathEpoch atomically bumps and reads the durable counter for path,
// starting at 1 for a path never seen before. Runs inside the caller's tx —
// AcquireLocks always calls this under the same tx it inserts the lock row in,
// so a crash between the two can never leave the counter ahead of what any
// lock row actually claims.
func nextPathEpoch(ctx context.Context, tx *sql.Tx, path string) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, `
INSERT INTO path_epochs(path_canonical, epoch) VALUES (?, 1)
ON CONFLICT(path_canonical) DO UPDATE SET epoch = epoch + 1
RETURNING epoch`, path).Scan(&epoch)
	return epoch, err
}

// resolveEpoch decides whether l's acquisition is a RENEWAL (same owner
// already holds a LIVE row at this exact target — preserve its epoch) or a
// fresh GRANT (no such row, or the row is stale — bump path_epochs). all is
// the pre-tx lock snapshot AcquireLocks already loaded; ec supplies the same
// staleness verdict every other predicate in this file consults.
//
// ‡ Why scanning `all` for PRESENCE is a reliable signal, not a race:
// reclaimStaleAndCollectBlockers (locks_acquire.go) skips same-owner rows
// outright (`ex.OwnerUUID == l.OwnerUUID → continue`) when it deletes stale
// rows earlier in the SAME tx, regardless of their own staleness — so a
// same-owner row present in `all` is guaranteed to still be in the DB when
// insertOrRefreshLock's upsert runs a few lines later. Present ⟹ the SQL will
// hit ON CONFLICT DO UPDATE. Absent ⟹ it will INSERT.
//
// ‡ Why STALENESS still gates the epoch decision even though it doesn't gate
// the SQL branch: a same-owner row surviving reclaim's skip does not mean it
// is LIVE — an owner can re-acquire its own lock after the TTL lapsed with no
// staleness check applied (self-locks never conflict). But a lapsed TTL is
// precisely "territory became reclaimable" — the plan's own words are
// "stale-owner reclaim... increment[s]", and a self-reclaim after expiry is
// still a reclaim. Only a row that is both present AND live is a genuine
// renewal; present-but-stale takes the same fresh-grant path a stranger's
// reclaim would.
func resolveEpoch(ctx context.Context, tx *sql.Tx, l domain.LockRecord, all []domain.LockRecord, ec domain.EvalContext) (int64, error) {
	for i := range all {
		if all[i].OwnerUUID == l.OwnerUUID && domain.SameCanonical(all[i].Target, l.Target) && !ec.IsStale(all[i]) {
			return all[i].Epoch, nil // renewal: preserve
		}
	}
	return nextPathEpoch(ctx, tx, l.Target.Canonical) // fresh grant: bump
}
