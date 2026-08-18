package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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

	if err := validateAllFileTargets(sorted); err != nil {
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

	blockers, err := collectAllBlockers(ctx, tx, all, sorted, now, live)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		return nil, &MultiConflictError{Blockers: blockers}
	}

	if err := s.insertAllLocks(ctx, tx, sorted, all, domain.EvalContext{Now: now, Live: live}); err != nil {
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
//
// A beacon that yields to a stronger same-owner row (see insertOrRefreshLock)
// writes nothing, so it emits no lock_acquired event either — the audit trail
// records lock acquisitions, not attempts.
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
		written, err := insertOrRefreshLock(ctx, tx, sorted[i], beaconMaySupersede(sorted[i], all, ec), epoch)
		if err != nil {
			return err
		}
		if !written {
			continue
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

func validateAllFileTargets(sorted []domain.LockRecord) error {
	for i := range sorted {
		if err := validateFileTarget(sorted[i].Target.Canonical); err != nil {
			return err
		}
	}
	return nil
}

func validateFileTarget(p string) error {
	lst, err := os.Lstat(p)
	if err != nil {
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
func collectAllBlockers(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, sorted []domain.LockRecord, now time.Time, live domain.HolderLiveProbe) ([]domain.LockRecord, error) {
	// Bundle the (now, live) ambient pair once. Host policy rides inside the
	// probe closure (HolderLiveProbe takes the record), so one EvalContext
	// serves every lock in the batch — no per-lock rebinding.
	ec := domain.EvalContext{Now: now, Live: live}
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
		if !domain.SameCanonical(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID {
			continue
		}
		if ec.IsStale(*ex) {
			if err := reclaimStaleTx(ctx, tx, *ex, string(l.OwnerUUID), ec.Now); err != nil {
				return nil, err
			}
			continue
		}
		// Mode-aware: a shared peer does not block a shared acquire. The
		// same-canonical/same-owner/stale guards above are kept for the reclaim
		// side-effect; Conflicts is the final gate on whether a live, non-self
		// peer actually blocks (loto-k5el.2 T3).
		if ec.Conflicts(l, *ex) {
			blockers = append(blockers, all[i])
		}
	}
	return blockers, nil
}

// beaconMaySupersede reports whether an incoming BEACON is allowed to overwrite
// the same-owner row already at its target — true when there is no such row, or
// when the row is stale by the one staleness predicate (TTL lapsed OR holder
// provably dead).
//
// ‡ Same owner does not mean same holder (Codex #252). Sibling sessions sharing
// one LOTO_AGENT_ID are a supported, tested configuration (loto-81n), so an
// unexpired explicit lock under this owner uuid can belong to a sibling that has
// since died. collectAllBlockers skips every same-owner row, so nothing else in
// this transaction will reclaim it: yield to it and the dead sibling's row is
// what peers see, they probe it DEAD and reclaim it, and one of them writes the
// file concurrently with the session that just tried to beacon it. Asking the
// probe — the same one `check --gate` and `guard` consult — is what keeps every
// consumer on one verdict.
//
// The probe is called at most once per target here, and only for beacons that
// actually collide with an existing row.
func beaconMaySupersede(l domain.LockRecord, all []domain.LockRecord, ec domain.EvalContext) bool {
	if !l.IsBeacon() {
		return true // not a beacon: the yield does not apply at all
	}
	for i := range all {
		ex := &all[i]
		if ex.OwnerUUID != l.OwnerUUID || !domain.SameCanonical(ex.Target, l.Target) {
			continue
		}
		return ec.IsStale(*ex)
	}
	return true // no same-owner row to yield to
}

// insertOrRefreshLock upserts one lock row and reports whether the row was
// actually written. false means the beacon yield below suppressed the update:
// no error, nothing changed, and the caller must not log an acquisition.
// supersede comes from beaconMaySupersede and lifts the yield. epoch is the
// value resolveEpoch already decided — preserved on the UPDATE branch
// (deliberately absent from the SET list below) and only ever supplied fresh
// on the INSERT branch (loto-ovno.2).
func insertOrRefreshLock(ctx context.Context, tx *sql.Tx, l domain.LockRecord, supersede bool, epoch int64) (bool, error) {
	// Map 0 (UNKNOWN) → NULL at the store boundary so an absent start-time is a
	// SQL null, matching legacy rows. A refresh re-stamps proc_start because the
	// holder is the same process (same pid, same start-time).
	var procStart any
	if l.ProcStart != 0 {
		procStart = l.ProcStart
	}
	// ON CONFLICT targets the composite PK (target_canonical, owner_uuid) added in
	// loto-k5el.2 — so a same-owner re-acquire upserts its single row while a
	// different owner inserts a coexisting row (multi-holder). The old
	// `WHERE locks.owner_uuid = excluded.owner_uuid` guard is now redundant (the
	// conflict is keyed on owner) and dropped. Persist EffectiveMode() (not raw
	// l.Mode) so the column never stores '' (loto-k5el.2 T3).
	//
	// ‡ The DO UPDATE WHERE is the beacon yield (loto-xl4g, Codex #249): an
	// incoming BEACON never overwrites an existing LIVE NON-beacon row of the
	// same owner. Without it, the gate minting a beacon for an agent that
	// already ran `loto lock` rewrote that agent's own row to shared / pid 0 /
	// no branch / 2m — silently downgrading a declared exclusive 30m lease and
	// then letting `loto guard` waive it as a beacon and move the tree out from
	// under uncommitted work. A beacon says "an agent of mine is writing here";
	// a row that already says something stronger needs no weakening. The
	// converse still applies: an explicit lock upgrades over a beacon, because
	// then excluded.beacon is 0 and the update runs.
	//
	// ‡ supersede is the yield's escape hatch, decided by beaconMaySupersede
	// under the same staleness predicate every other consumer uses (Codex
	// #252). Yielding to a STALE lease protects nothing: collectAllBlockers
	// skips same-owner rows, so a stale explicit row is neither reclaimed nor
	// refreshed, and peers read it as free — `loto beacon` would report success
	// over a row announcing the file is available while the agent is mid-edit.
	res, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, proc_start, branch, mode, beacon, epoch)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical, owner_uuid) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  proc_start=excluded.proc_start,
  branch=excluded.branch,
  mode=excluded.mode,
  beacon=excluded.beacon
WHERE ? = 1
   OR excluded.beacon = 0
   OR locks.beacon = 1`,
		l.Target.Canonical, string(l.OwnerUUID), string(l.SessionUUID),
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, procStart, l.Branch, l.EffectiveMode(), l.Beacon, epoch,
		supersede,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
