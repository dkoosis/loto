package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"loto/internal/domain"
)

// sortedByCanonical returns a copy of recs ordered by canonical target path,
// the deterministic lock-acquisition order that prevents ABBA deadlocks.
func sortedByCanonical(recs []domain.LockRecord) []domain.LockRecord {
	sorted := make([]domain.LockRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Target.Canonical < sorted[j].Target.Canonical
	})
	return sorted
}

// AcquireLocks acquires a batch of locks in one transaction under the project
// op-flock.
//
// PRECONDITION: every record in recs must carry the SAME OwnerUUID. The reclaim
// breadcrumbs attribute the whole batch to sorted[0].OwnerUUID, so a
// mixed-owner batch would name the wrong actor on those events (loto-13pk). The
// loto CLI always builds single-owner batches (one agent per invocation via
// buildLockRecords from rt.Agent.UUID), so the precondition holds today; a
// future batch-import/migration caller that submits mixed owners must thread
// the per-record owner through before relying on these audit events.
func (s *Store) AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.HolderLiveProbe) ([]domain.LockRecord, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	sorted := sortedByCanonical(recs)

	flock, err := acquireOpFlockFn(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	if err := s.validateAllFileTargets(sorted); err != nil {
		return nil, err
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	// Acquisition-time overlap block (loto-ovno.2 part 3, git-gate.md "Claim
	// lifecycle"): a new lease must not land on a path an unresolved candidate
	// claim already covers, or the candidate's captured preimage is guaranteed
	// stale the moment this acquire's holder edits — a rejection this cheap
	// check could have caught at lock time instead. Checked before the
	// ordinary lock-conflict scan; both reads are equally cheap, and failing
	// on the claim first means a caller sees "a candidate is pending here"
	// rather than a same-shaped-but-unrelated lock-conflict message.
	if err := blockOnCandidateClaims(ctx, tx, sorted); err != nil {
		return nil, err
	}

	// Bundle the (now, live) ambient pair once. Host policy rides inside
	// the probe closure (HolderLiveProbe takes the record), so one EvalContext
	// serves every lock in the batch — no per-lock rebinding.
	blockers, err := collectAllBlockers(ctx, tx, all, sorted, domain.EvalContext{Now: now, Live: live, CaseFold: s.caseFold})
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return nil, &MultiConflictError{Blockers: blockers}
	}

	if err := s.insertAllLocks(ctx, tx, sorted, all, domain.EvalContext{Now: now, Live: live, CaseFold: s.caseFold}); err != nil {
		return nil, err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return nil, err
	}
	if err := commitTxFn(tx); err != nil {
		return nil, err
	}

	// loto-zssw: nothing to undo post-commit. Acquire no longer alters mode
	// bits, so there is no stripped set, no restore, and none of the flock
	// ordering that existed only to make the restore safe. The deferred
	// flock.release() carries it.
	return sorted, nil
}

// insertAllLocks writes the lock rows and their lock_acquired events inside
// the parent tx. On error the caller (AcquireLocks) releases the tx and runs
// no compensating filesystem action, so failures here just propagate the error.
func (s *Store) insertAllLocks(ctx context.Context, tx *sql.Tx, sorted, all []domain.LockRecord, ec domain.EvalContext) error {
	evs := make([]domain.Event, 0, len(sorted))
	for i := range sorted {
		epoch, err := resolveEpoch(ctx, tx, sorted[i], all, ec)
		if err != nil {
			return err
		}
		// Written back onto the shared backing array so the caller's returned
		// records (AcquireLocks' `return sorted, nil`) carry the epoch the row
		// was actually persisted under, not the zero value the caller built it
		// with — a caller minting a candidate envelope's LeaseEpoch reads this.
		sorted[i].Epoch = epoch
		if err := insertOrRefreshLock(ctx, tx, sorted[i], epoch, keysFor(ec)); err != nil {
			return err
		}
		evs = append(evs, domain.Event{
			Target:    sorted[i].Target,
			Kind:      EventLockAcquired,
			ActorUUID: string(sorted[i].OwnerUUID),
			Reason:    sorted[i].Intent,
			CreatedAt: ec.Now,
		})
	}
	// Emit lock_acquired events in the same tx (atomic with the row inserts).
	return appendEventsTx(ctx, tx, evs)
}

// blockOnCandidateClaims refuses the whole batch if ANY target overlaps an
// unresolved candidate claim, sorted deterministically by (path, candidate)
// for a stable error. No liveness reclaim happens here: the store's job is to
// say a claim exists, not to decide it is abandoned — that judgment (via
// domain.EvalContext.CandidateClaimIsDead) belongs to whichever later bead
// actively reclaims stale candidate claims, kept separate from every ordinary
// caller's lock acquisition path on purpose.
func blockOnCandidateClaims(ctx context.Context, tx *sql.Tx, sorted []domain.LockRecord) error {
	paths := make([]string, len(sorted))
	for i := range sorted {
		paths[i] = sorted[i].Target.Canonical
	}
	claims, err := candidateClaimsForPathsTx(ctx, tx, paths)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		return nil
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].PathCanonical != claims[j].PathCanonical {
			return claims[i].PathCanonical < claims[j].PathCanonical
		}
		return claims[i].CandidateID < claims[j].CandidateID
	})
	return &CandidateClaimConflictError{Blockers: claims}
}

func (s *Store) validateAllFileTargets(sorted []domain.LockRecord) error {
	for i := range sorted {
		if err := validateFileTarget(s.repoTop, sorted[i]); err != nil {
			return err
		}
	}
	return nil
}

// validateFileTarget checks rec's target path exists as a regular,
// non-multi-linked, non-symlink file — except that a record the caller marked
// MayCreate (the pre-hook admitting a Write that creates its file, loto-9zcq,
// loto-z5nb) may name a path that does not exist yet. ENOENT only: a path that
// DOES exist, or a plain `loto lock` on any missing path, still runs every
// check below; a symlink or non-regular file is always refused.
//
// ‡ The canonical is repo-relative, so it is joined to repoTop before it is
// stat'd (loto-3tv3 D8). Probing it bare resolves against the process CWD,
// which is right only from the repo root: from a subdirectory the store either
// misses the file entirely or stats a same-named neighbour. Errors and
// TargetValidationError.Path still name the CANONICAL, matching
// statFileTargetReason's precedent — the caller thinks in keys, not probes.
func validateFileTarget(repoTop string, rec domain.LockRecord) error {
	p := rec.Target.Canonical
	probe := p
	if repoTop != "" && !filepath.IsAbs(p) {
		probe = filepath.Join(repoTop, filepath.FromSlash(p))
	}
	lst, err := os.Lstat(probe)
	if err != nil {
		if rec.MayCreate && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("validate %s: %w", p, err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return &TargetValidationError{Path: p, Reason: ReasonSymlink}
	}
	if !lst.Mode().IsRegular() {
		return &TargetValidationError{Path: p, Reason: ReasonNotRegular}
	}
	if sys, ok := lst.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return &TargetValidationError{Path: p, Reason: ReasonMultiLinked, Nlink: uint64(sys.Nlink)}
	}
	return nil
}

// collectAllBlockers returns the live conflicting holders plus the canonical
// paths of reclaimed stale EXCLUSIVE rows (deduped) — the caller must restore
// owner-write on those after commit unless it re-stripped them itself.
func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, ec domain.EvalContext) ([]domain.LockRecord, error) {
	seen := map[string]bool{}
	var blockers []domain.LockRecord
	for i := range sorted {
		bs, err := reclaimStaleAndCollectBlockers(ctx, tx, all, sorted[i], ec)
		if err != nil {
			return nil, err
		}
		for j := range bs {
			key := string(bs[j].OwnerUUID) + "|" + bs[j].Target.Canonical
			if !seen[key] {
				seen[key] = true
				blockers = append(blockers, bs[j])
			}
		}
	}
	sort.Slice(blockers, func(i, j int) bool {
		if !blockers[i].CreatedAt.Equal(blockers[j].CreatedAt) {
			return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
		}
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	return blockers, nil
}

// reclaimStaleAndCollectBlockers deletes stale rows contending with l and
// returns the surviving blockers. Reclaim used to also report which paths
// needed their owner-write bit put back; nothing strips it any more
// (loto-zssw), so the reclaim is a pure row delete.
func reclaimStaleAndCollectBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, l domain.LockRecord, ec domain.EvalContext) ([]domain.LockRecord, error) {
	var blockers []domain.LockRecord
	for i := range all {
		ex := &all[i]
		// Same-owner rows are never reclaimed here, never blocking. A row from a sibling worktree is a different file on disk
		// (loto-3eq6): never reclaimed from here, never blocking.
		if !ec.SameTarget(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID ||
			!domain.SameWorktree(l.Worktree, ex.Worktree) {
			continue
		}
		if ec.IsStale(*ex) {
			if err := reclaimStaleTx(ctx, tx, *ex, string(l.OwnerUUID), ec.Now); err != nil {
				return nil, err
			}
			continue
		}
		// The same-canonical/same-owner/stale guards above are kept for the
		// reclaim side-effect; Conflicts is the final gate on whether a live,
		// non-self peer actually blocks.
		if ec.Conflicts(l, *ex) {
			blockers = append(blockers, all[i])
		}
	}
	return blockers, nil
}

// adoptLegacyWorktreeRow stamps l's worktree onto the same owner's legacy
// row at l's target — one written before locks.worktree existed, or by a store
// with no repo frame, so its worktree is empty — when l names a worktree
// (loto-8z87, PR #373 review). Under the old (target_canonical, owner_uuid)
// key that re-acquire refreshed the legacy row, worktree included; under the
// widened key it would INSERT a second row beside it, and worktreeFilter
// matches both, so owner lookups and releases lose their one-row answer.
// UPDATE OR IGNORE leaves the legacy row alone when this worktree already has
// its own row: then the upsert below refreshes that one, as before.
//
// The target is the name the caller typed, so it matches under k's fold, the
// same as resolveEpoch's SameTarget (PR #373 review): on a case-folding store
// a legacy Foo.go row is the file the CLI now names foo.go. Adoption also
// rewrites target_canonical to the caller's key, because the upsert's ON
// CONFLICT is byte-exact and would otherwise insert beside the adopted row.
func adoptLegacyWorktreeRow(ctx context.Context, tx *sql.Tx, l domain.LockRecord, k keyMatch) error {
	if l.Worktree == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE OR IGNORE locks SET worktree = ?, target_canonical = ? WHERE `+k.col("target_canonical")+` = ? AND owner_uuid = ? AND worktree = ''`, //nolint:gosec // G202 k.col renders a fixed column expression, all data via args
		l.Worktree, l.Target.Canonical, k.key(l.Target.Canonical), string(l.OwnerUUID))
	return err
}

// insertOrRefreshLock upserts one lock row. epoch is the
// value resolveEpoch already decided, and BOTH branches write it (loto-ovno.2,
// corrected by loto-rbij): resolveEpoch is the sole renewal-vs-fresh-grant
// authority, handing back the row's existing epoch for a genuine renewal and a
// freshly bumped path_epochs value for a reclaim. Omitting epoch from the SET
// list did not "preserve" it — it pinned the row to its pre-lapse generation
// while the counter moved on, so a same-owner re-acquire after its OWN TTL
// lapsed left row and counter permanently divergent and an epoch check read a
// stale envelope as current.
func insertOrRefreshLock(ctx context.Context, tx *sql.Tx, l domain.LockRecord, epoch int64, k keyMatch) error {
	// Map 0 (UNKNOWN) → NULL at the store boundary so an absent start-time is a
	// SQL null, matching legacy rows. A refresh re-stamps proc_start because the
	// holder is the same process (same pid, same start-time).
	var procStart any
	if l.ProcStart != 0 {
		procStart = l.ProcStart
	}
	if err := adoptLegacyWorktreeRow(ctx, tx, l, k); err != nil {
		return err
	}
	// ON CONFLICT targets the composite PK (target_canonical, owner_uuid,
	// worktree) — owner_uuid added in loto-k5el.2, worktree added in loto-8z87
	// — so a same-owner, same-worktree re-acquire upserts its single row while
	// a different owner OR a different worktree under the same owner inserts a
	// coexisting row (multi-holder, or one owner's independent copies of a
	// path in two linked worktrees). Before loto-8z87, worktree was a plain
	// column outside the conflict target: a same-owner acquire from a
	// DIFFERENT worktree still matched the old two-column key and silently
	// overwrote the first worktree's row instead of inserting beside it. The
	// old `WHERE locks.owner_uuid = excluded.owner_uuid` guard is now
	// redundant (the conflict is keyed on owner) and dropped.
	_, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, proc_start, branch, epoch, worktree)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical, owner_uuid, worktree) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  proc_start=excluded.proc_start,
  branch=excluded.branch,
  epoch=excluded.epoch`,
		l.Target.Canonical, string(l.OwnerUUID), string(l.SessionUUID),
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, procStart, l.Branch, epoch, l.Worktree,
	)
	return err
}
