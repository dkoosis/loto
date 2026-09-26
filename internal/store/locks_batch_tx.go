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

	existing, err := loadLocksByTargetTx(ctx, tx, s.keys(), targets)
	if err != nil {
		return err
	}
	scopeToWorktree(existing, s.repoTop)

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

// scopeToWorktree drops, in place, every row whose Worktree names a checkout
// other than repoTop (loto-3eq6). The store is shared by every linked
// worktree of one repo, so such a row is a different file on disk: a release
// or `unlock --force` issued from this checkout has no standing over it, and
// must neither delete it nor let it veto (PR #372 review). An unset side
// matches anything (domain.SameWorktree), so a legacy row and a store opened
// without a repo top keep today's reach.
func scopeToWorktree(existing map[string][]domain.LockRecord, repoTop string) {
	for k, rows := range existing {
		kept := rows[:0]
		for i := range rows {
			if domain.SameWorktree(repoTop, rows[i].Worktree) {
				kept = append(kept, rows[i])
			}
		}
		if len(kept) == 0 {
			delete(existing, k)
			continue
		}
		existing[k] = kept
	}
}

// worktreeFilter returns a SQL predicate plus its two positional args that
// mirrors domain.SameWorktree(worktree, locks.worktree) inside a raw query —
// loto-8z87. True when the caller's own worktree is unset ("no repo frame",
// today's reach), the row's is unset (a legacy row, read as "unknown" and
// matched conservatively), or the two are byte-equal. Never narrower than
// domain.SameWorktree, so a caller that already scoped an in-memory snapshot
// with that predicate (scopeToWorktree above) gets the identical answer from
// a raw DELETE/UPDATE/SELECT built with this clause. The two `?` placeholders
// both bind worktree; append the returned args once, in clause order.
func worktreeFilter(worktree string) (string, []any) {
	return `(? = '' OR worktree = '' OR worktree = ?)`, []any{worktree, worktree}
}
