package store

import (
	"context"

	"loto/internal/domain"
)

// WithStableCoordinationState holds the project operation flock while it reads
// every coordination row relevant to paths and while inspect acts on that
// snapshot. Lock, territory-claim, and candidate-claim acquisitions use the
// same flock, so none can land after these reads and before inspect returns.
//
// inspect must not call a Store method that acquires the operation flock. It
// may perform the filesystem mutation authorized by the snapshot; keeping that
// mutation inside the callback is what closes the read-to-write TOCTOU window.
func (s *Store) WithStableCoordinationState(
	ctx context.Context,
	paths []string,
	inspect func([]domain.LockRecord, []domain.ClaimRecord, []domain.CandidateClaim) error,
) error {
	flock, err := acquireOpFlockFn(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	locks, err := s.ListLocks(ctx)
	if err != nil {
		return err
	}
	claims, err := s.ListClaims(ctx)
	if err != nil {
		return err
	}
	candidates, err := s.CandidateClaimsForPaths(ctx, paths)
	if err != nil {
		return err
	}
	return inspect(locks, claims, candidates)
}
