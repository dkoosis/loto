package store

import (
	"context"
	"fmt"
	"io"

	"loto/internal/domain"
)

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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit candidate claims: %w", err)
	}
	return nil
}

// setStderr overrides the writer used for diagnostic messages (audit-write
// failures, op-flock contention notices). Defaults to os.Stderr. Intended for
// tests that need to observe these messages; production code should keep the
// default.
func (s *Store) setStderr(w io.Writer) { s.stderr = w }

// Open opens the loto store at path. On a real sqlite corruption error
// (SQLITE_CORRUPT or SQLITE_NOTADB, errno-checked — not string-matched),
// the existing DB and its -wal/-shm siblings are moved aside atomically
// and a fresh DB is created. Recovery is serialized via flock on a
// sidecar lock file so concurrent openers cannot interleave.
//
// First-Open is serialized on the project op-flock: two processes
// creating the same DB simultaneously would otherwise both pass the
// existence check and clobber each other's writes. Subsequent Opens on
// an initialized DB take the fast path (no flock).
func Open(p string) (*Store, error) {
	return OpenContext(context.Background(), p)
}
