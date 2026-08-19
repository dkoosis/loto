package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"loto/internal/domain"
)

// candidateClaimsDDL is the candidate_claims table shape, duplicated from
// schema.sql per the claimsDDL precedent: a pending ensureFn must be able to
// apply itself, even though the usual delivery path is migrate's schemaSQL
// pass.
const candidateClaimsDDL = `
CREATE TABLE IF NOT EXISTS candidate_claims (
  path_canonical TEXT NOT NULL,
  candidate_id   TEXT NOT NULL,
  owner_uuid     TEXT NOT NULL,
  session_uuid   TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  host           TEXT NOT NULL DEFAULT '',
  pid            INTEGER NOT NULL DEFAULT 0,
  proc_start     INTEGER,
  PRIMARY KEY (path_canonical, candidate_id)
);
CREATE INDEX IF NOT EXISTS idx_candidate_claims_candidate ON candidate_claims(candidate_id);`

const candidateClaimCols = `path_canonical, candidate_id, owner_uuid, session_uuid, created_at, host, pid, proc_start`

// ClaimLeaseConflict is one write-set path whose lease no longer backs the
// candidate at claim-insert time.
type ClaimLeaseConflict struct {
	Path   string
	Reason string
}

// Conflict reasons a claim-time revalidation can report. Each maps onto the
// git-gate.md stale-lease-epoch rejection class — they differ only in WHICH
// way the authorization stopped holding, which is what the proposer needs to
// read to know whether to re-lock or give up.
const (
	ClaimLeaseGone         = "lease-gone"
	ClaimLeaseStale        = "lease-stale"
	ClaimLeaseNotExclusive = "lease-not-exclusive"
	ClaimLeaseEpochChanged = "lease-epoch-changed"
)

// LeaseRevalidationError reports that the leases a candidate was admitted
// under no longer hold at the moment its claims would land. Conflicts is
// sorted by path so the same state renders byte-identically.
type LeaseRevalidationError struct {
	Conflicts []ClaimLeaseConflict
}

func (e *LeaseRevalidationError) Error() string {
	if len(e.Conflicts) == 1 {
		return fmt.Sprintf("loto: lease revalidation failed: %s: %s",
			e.Conflicts[0].Path, e.Conflicts[0].Reason)
	}
	return fmt.Sprintf("loto: lease revalidation failed on %d paths (first: %s: %s)",
		len(e.Conflicts), e.Conflicts[0].Path, e.Conflicts[0].Reason)
}

// ClaimGuard is the lease state a candidate's claims must still find true at
// insert time. Owner and Epoch come from the envelope the gate admitted; Live
// is the same liveness probe the lock path uses, so "my lease is still good"
// means exactly what it means everywhere else in loto.
type ClaimGuard struct {
	Owner domain.AgentUUID
	Epoch map[string]int64
	Live  domain.HolderLiveProbe
}

// InsertCandidateClaims revalidates guard against the live lock table and
// writes one row per path in claims — both inside ONE tx under the project
// op-flock, which is the whole point (loto-ovno.10, Codex #259 P1).
//
// ‡ The check cannot live in the CLI. runSubmit reads each path's epoch, calls
// gate.Admit, and only then asks the store to claim; nothing held the flock
// across that gap, so a lease that expired, was force-broken, or was regranted
// inside the window let AcquireLocks commit a PEER's lock first while
// admission was still judging on the stale epoch map — leaving a live peer
// lock AND this candidate's claim on the same path. Holding the same flock
// AcquireLocks holds, and re-reading the leases in the same tx that inserts,
// makes the two operations serialize: whichever gets the flock second sees the
// other's committed effect and loses cleanly.
//
// A crash mid-write can still never leave some paths claimed and others free
// (one tx), and no overlap check runs here: refusing a new lease that overlaps
// an unresolved claim is the ACQUISITION side's job (blockOnCandidateClaims).
// Two candidates legitimately claiming disjoint paths is the whole point of
// concurrent candidates.
func (s *Store) InsertCandidateClaims(ctx context.Context, claims []domain.CandidateClaim, guard ClaimGuard) error {
	if len(claims) == 0 {
		return nil
	}
	flock, err := acquireOpFlockFn(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := revalidateClaimLeasesTx(ctx, tx, claims, guard, time.Now()); err != nil {
		return err
	}

	if err := insertCandidateClaimRowsTx(ctx, tx, claims); err != nil {
		return err
	}
	return commitTxFn(tx)
}

// insertCandidateClaimsUnguarded writes the rows with no lease revalidation.
// The store's own tests use it to plant claim fixtures whose subject is
// something else (the acquisition-time overlap block, list/release round
// trips); production has exactly one door, the guarded
// InsertCandidateClaims above.
func (s *Store) insertCandidateClaimsUnguarded(ctx context.Context, claims []domain.CandidateClaim) error {
	if len(claims) == 0 {
		return nil
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := insertCandidateClaimRowsTx(ctx, tx, claims); err != nil {
		return err
	}
	return tx.Commit()
}

// insertCandidateClaimRowsTx writes one row per claim inside the caller's tx —
// a candidate's write-set is claimed atomically or not at all, so a crash
// mid-write can never leave some paths claimed and others free for a
// concurrent acquire to slip into.
func insertCandidateClaimRowsTx(ctx context.Context, tx *sql.Tx, claims []domain.CandidateClaim) error {
	for i := range claims {
		c := &claims[i]
		var procStart any
		if c.ProcStart != 0 {
			procStart = c.ProcStart
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO candidate_claims(`+candidateClaimCols+`) VALUES (?,?,?,?,?,?,?,?)`,
			c.PathCanonical, c.CandidateID, string(c.OwnerUUID), string(c.SessionUUID),
			c.CreatedAt.UnixNano(), c.Host, c.PID, procStart,
		); err != nil {
			return err
		}
	}
	return nil
}

// revalidateClaimLeasesTx re-reads the lock table inside the claiming tx and
// reports every write-set path whose authorization no longer holds.
//
// Requiring the submitter's OWN live exclusive lease at the pinned epoch is
// sufficient to exclude a peer lock on the same path: AcquireLocks takes the
// same op-flock and refuses to grant over a live exclusive lock, so a peer row
// can only exist here if this owner's row is gone, stale, or was replaced —
// each of which this check already rejects.
func revalidateClaimLeasesTx(ctx context.Context, tx *sql.Tx, claims []domain.CandidateClaim, guard ClaimGuard, now time.Time) error {
	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return err
	}
	held := make(map[string]domain.LockRecord, len(all))
	for i := range all {
		if all[i].OwnerUUID == guard.Owner {
			held[all[i].Target.Canonical] = all[i]
		}
	}
	ec := domain.EvalContext{Now: now, Live: guard.Live}

	var conflicts []ClaimLeaseConflict
	for i := range claims {
		path := claims[i].PathCanonical
		l, ok := held[path]
		switch {
		case !ok:
			conflicts = append(conflicts, ClaimLeaseConflict{path, ClaimLeaseGone})
		case ec.IsStale(l):
			conflicts = append(conflicts, ClaimLeaseConflict{path, ClaimLeaseStale})
		case l.EffectiveMode() != domain.ModeExclusive:
			conflicts = append(conflicts, ClaimLeaseConflict{path, ClaimLeaseNotExclusive})
		case guard.Epoch[path] != l.Epoch:
			conflicts = append(conflicts, ClaimLeaseConflict{path, ClaimLeaseEpochChanged})
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return &LeaseRevalidationError{Conflicts: conflicts}
}

// ReleaseCandidateClaims deletes every row for candidateID — the store-layer
// half of "released on promoted/rejected/withdrawn" (git-gate.md). The store
// does not distinguish which of the three happened; that is audit-worthy
// information the CALLER holds (an admission/promotion bead emits its own
// event for the outcome, mirroring how BreakLocks takes a reason string
// rather than the store inventing one).
func (s *Store) ReleaseCandidateClaims(ctx context.Context, candidateID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM candidate_claims WHERE candidate_id = ?`, candidateID)
	return err
}

// ListCandidateClaims returns every candidate claim, expired-liveness
// included — staleness is a caller-time judgment via
// domain.EvalContext.CandidateClaimIsDead, mirroring ListClaims/ListLocks'
// read-all-then-filter contract. Deterministic order (path, candidate_id) so
// the same DB renders byte-identically.
func (s *Store) ListCandidateClaims(ctx context.Context) ([]domain.CandidateClaim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+candidateClaimCols+` FROM candidate_claims ORDER BY path_canonical, candidate_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidateClaims(rows)
}

// CandidateClaimsForPaths returns claims covering any of the given canonical
// paths — the query AcquireLocks' overlap block runs before granting a new
// lease (part 3 of loto-ovno.2: "lease acquisition fails on overlap with any
// unresolved candidate claim... fail at the cheap end").
func (s *Store) CandidateClaimsForPaths(ctx context.Context, paths []string) ([]domain.CandidateClaim, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	placeholders, args := inClauseStrings(paths)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+candidateClaimCols+` FROM candidate_claims WHERE path_canonical IN (`+placeholders+`)`+ //nolint:gosec // G202 placeholders are '?' chars only, all data via args
			` ORDER BY path_canonical, candidate_id`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidateClaims(rows)
}

// candidateClaimsForPathsTx is CandidateClaimsForPaths' tx-scoped twin, for the
// acquisition-time overlap block (locks_acquire.go): it must read inside the
// SAME tx that is about to grant a new lease, under the SAME op-flock, or a
// candidate claim minted between the read and the write would go unseen.
func candidateClaimsForPathsTx(ctx context.Context, tx *sql.Tx, paths []string) ([]domain.CandidateClaim, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	placeholders, args := inClauseStrings(paths)
	rows, err := tx.QueryContext(ctx,
		`SELECT `+candidateClaimCols+` FROM candidate_claims WHERE path_canonical IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidateClaims(rows)
}

func scanCandidateClaims(rows *sql.Rows) ([]domain.CandidateClaim, error) {
	var out []domain.CandidateClaim
	for rows.Next() {
		var c domain.CandidateClaim
		var owner, session string
		var createdNs int64
		var procStart sql.NullInt64
		if err := rows.Scan(&c.PathCanonical, &c.CandidateID, &owner, &session,
			&createdNs, &c.Host, &c.PID, &procStart); err != nil {
			return nil, err
		}
		c.OwnerUUID = domain.AgentUUID(owner)
		c.SessionUUID = domain.SessionUUID(session)
		c.CreatedAt = time.Unix(0, createdNs).UTC()
		if procStart.Valid {
			c.ProcStart = procStart.Int64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// claimCompensateTimeout bounds the compensating claim delete AcceptCandidate
// runs when the git side of an acceptance fails after the claims already
// committed. Same shape and rationale as auditDetachedTimeout.
const claimCompensateTimeout = 2 * time.Second

// releaseCandidateClaimsForPathsDetached undoes exactly the rows one
// AcceptCandidate attempt inserted, on a context detached from the caller's.
//
// ‡ Detached (Codex #261 P1): the usual reason WriteBlob/WriteCandidateRefs
// fails is that the caller went away — Ctrl-C, a cancelled ctx. Compensating
// on that same cancelled ctx fails too, and the claims stay on disk with no
// candidate refs behind them. Acquisition treats every such claim as
// unresolved and there is no stale-claim reclamation path yet, so the write
// set would be blocked for good.
//
// ‡ Path-scoped, not candidate-scoped (Codex #261 P2): a candidate-wide
// DELETE also removes rows an earlier accepted candidate holds under the same
// ID. NewCandidateID makes that collision vanishingly unlikely today, but the
// compensation's contract is "undo what THIS attempt wrote," and scoping to
// the write set states that in the query instead of resting on ID uniqueness.
func (s *Store) releaseCandidateClaimsForPathsDetached(candidateID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), claimCompensateTimeout) //nolint:forbidigo // compensating delete must outlive the (cancelled) acceptance, else the claims strand with no refs behind them.
	defer cancel()

	placeholders, args := inClauseStrings(paths)
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM candidate_claims WHERE candidate_id = ? AND path_canonical IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		append([]any{candidateID}, args...)...)
	if err != nil && s.stderr != nil {
		fmt.Fprintf(s.stderr, "loto: releasing %d claim(s) for candidate %s failed: %v\n", len(paths), candidateID, err)
	}
	return err
}
