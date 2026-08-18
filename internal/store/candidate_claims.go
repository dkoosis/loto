package store

import (
	"context"
	"database/sql"
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

// InsertCandidateClaims writes one row per path in claims, all in one tx — a
// candidate's write-set is claimed atomically or not at all, so a crash
// mid-write can never leave some paths claimed and others free for a
// concurrent acquire to slip into (the exact hole part 3 of loto-ovno.2
// closes at the OTHER end, in AcquireLocks).
//
// No overlap check here: that is the ACQUISITION side's job (a new lease must
// not overlap an unresolved claim), not the claim side's. Two candidates
// legitimately claiming disjoint paths is the whole point of concurrent
// candidates; a later bead (admission, loto-ovno.4) is what decides whether
// THIS candidate was even eligible to reach here.
func (s *Store) InsertCandidateClaims(ctx context.Context, claims []domain.CandidateClaim) error {
	if len(claims) == 0 {
		return nil
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

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
	return tx.Commit()
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
