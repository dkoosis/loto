package store

import (
	"context"
	"errors"
	"time"

	"loto/internal/domain"
)

// ErrLeaseExpired reports a refresh attempt on a lock whose TTL has already
// lapsed. An expired lease is reclaimable territory — any peer's acquire deletes
// the row (reclaimStaleAndCollectBlockers, locks_acquire.go) without a doctor
// run. Extending it after the fact would silently un-free a target a peer may
// already be taking, so refresh refuses and the holder must re-acquire.
var ErrLeaseExpired = errors.New("lock lease already expired")

// RefreshResult is the per-target outcome from RefreshLocks, returned in input
// order. Err is nil on success, ErrNoLockAtTarget when the caller holds no lock
// at the target (including "a peer holds it"), or ErrLeaseExpired when the
// caller's own lease has already lapsed. ExpiresAt carries the new deadline on
// success and the untouched existing deadline otherwise.
type RefreshResult struct {
	Target    domain.Target
	Err       error
	ExpiresAt time.Time
}

// RefreshLocks pushes the expiry of each lock held by owner out to now+ttl, in
// place — same row, same created_at, no chmod (the write bit is already in the
// state the acquire mode dictates). This is the live half of the TTL lease:
// expiry frees territory a crashed holder can no longer defend, and a holder
// that is still alive proves it by refreshing before the deadline.
//
// Per-target failures do not abort the batch (see RefreshResult.Err); the
// returned error is non-nil only on internal/SQL failures. Mirrors
// DowngradeLocks: one op-flock for the whole batch, a plain read to decide, and
// a write tx opened only when at least one target actually needs the UPDATE
// (loto-kw5k fast path). No post-commit FS work, so the flock releases via the
// deferred call rather than the split restore-then-release dance.
//
// Callers pass distinct targets — the CLI dedups via validateLockTargets — so,
// like the sibling batch methods, this does not guard against duplicate input.
func (s *Store) RefreshLocks(ctx context.Context, targets []domain.Target, ownerID domain.AgentUUID, ttl time.Duration) ([]RefreshResult, error) {
	owner := string(ownerID) // internal store helpers thread the owner as a plain string
	if len(targets) == 0 {
		return []RefreshResult{}, nil
	}

	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	// Plain read under the held flock — the flock serializes lock mutators
	// across processes, so this snapshot is authoritative for the decision.
	held, err := s.LocksForOwnerAt(ctx, targets, ownerID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newExpiry := now.Add(ttl)
	results := make([]RefreshResult, len(targets))
	var extend []domain.Target
	var events []domain.Event
	for i, t := range targets {
		results[i].Target = t
		l, ok := held[t.Canonical]
		switch {
		case !ok:
			results[i].Err = ErrNoLockAtTarget
		case !l.ExpiresAt.After(now):
			results[i].Err = ErrLeaseExpired
			results[i].ExpiresAt = l.ExpiresAt
		default:
			results[i].ExpiresAt = newExpiry
			extend = append(extend, t)
			events = append(events, domain.Event{
				Target:    t,
				Kind:      EventLockRefreshed,
				ActorUUID: owner,
				Reason:    "ttl extended to " + newExpiry.Format(time.RFC3339),
				CreatedAt: now,
			})
		}
	}

	if len(extend) == 0 {
		return results, nil // nothing to write — never open the write tx
	}
	if err := s.commitRefreshes(ctx, extend, owner, newExpiry, events, now); err != nil {
		return nil, err
	}
	return results, nil
}

// commitRefreshes writes the batched expires_at UPDATE plus its lock_refreshed
// events in one immediate-mode write tx. The owner_uuid predicate is the
// authorization gate at the SQL layer too, not just in the caller's classify
// loop: a peer's row is unreachable even if the snapshot went stale under us.
// `expires_at > now` carries the ErrLeaseExpired invariant down with it — the
// op-flock makes both redundant today, and both stay so a caller that ever
// reaches this path unserialized still cannot resurrect a lapsed lease.
func (s *Store) commitRefreshes(ctx context.Context, extend []domain.Target, owner string, newExpiry time.Time, events []domain.Event, now time.Time) error {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	placeholders, canonArgs := inClause(extend)
	args := append([]any{newExpiry.UnixNano(), owner, now.UnixNano()}, canonArgs...)
	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET expires_at = ? WHERE owner_uuid = ? AND expires_at > ? AND target_canonical IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		args...); err != nil {
		return err
	}
	if err := appendEventsTx(ctx, tx, events); err != nil {
		return err
	}
	// Mirrors AcquireLocks/ReleaseLocks: any path that appends events rotates,
	// so a refresh-heavy heartbeat can't grow the events table unbounded.
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}
