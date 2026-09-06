package store

import (
	"context"
	"database/sql"
	"time"

	"loto/internal/domain"
)

// batchTxFn is the body of a batched lock mutation: it decides and writes,
// inside the transaction withLockBatchTx opened, against the rows and liveness
// context that transaction already read.
type batchTxFn func(tx *sql.Tx, existing map[string][]domain.LockRecord, ec domain.EvalContext, now time.Time) error

// withLockBatchTx runs one batched lock mutation under the project op-flock in
// a single transaction: acquire the flock, open the tx, load the current rows
// for every target, build the liveness context, run fn, commit, release.
//
// ‡ One home for the flock/tx lifecycle, not a line-count saving. ReleaseLocks
// and BreakLocks each carried this preamble verbatim, and the ordering it
// encodes is the load-bearing part: the flock is taken BEFORE the read, so the
// snapshot fn decides on cannot move under it; the flock outlives the commit,
// so a peer cannot observe a half-applied batch; and the deferred release is a
// backstop for every error path, with the explicit release on success keeping
// the hold as short as the work. A second copy of that ordering is a second
// place for it to drift.
//
// fn must not commit or release anything — it decides and writes, nothing more.
func (s *Store) withLockBatchTx(ctx context.Context, targets []domain.Target, live domain.HolderLiveProbe, fn batchTxFn) error {
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

	existing, err := loadLocksByTargetTx(ctx, tx, targets)
	if err != nil {
		return err
	}

	now := time.Now()
	if err := fn(tx, existing, domain.EvalContext{Now: now, Live: live, CaseFold: s.caseFold}, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	flock.release()
	return nil
}
