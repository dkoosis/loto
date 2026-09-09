package store

import (
	"context"
	"database/sql"
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

// hookRefreshReason marks a lock_refreshed event the pre-hook wrote on the
// caller's behalf, distinct from the "ttl extended to <ts>" reason a manual
// `loto refresh` leaves — the bead's AC greps for this exact token.
const hookRefreshReason = "hook"

// refreshCallerLocksTx is refreshOwnLocksBelowHalfTTLTx plus the retention
// sweep it earns only when it actually wrote something — split out of
// RecordCallPre so that call stays a single decision point on the hot path
// instead of two (loto-wkul; gocognit flagged the inlined form).
func refreshCallerLocksTx(ctx context.Context, tx *sql.Tx, owner string, now time.Time) error {
	refreshed, err := refreshOwnLocksBelowHalfTTLTx(ctx, tx, owner, now)
	if err != nil {
		return err
	}
	if len(refreshed) == 0 {
		return nil
	}
	return rotateEventsTx(ctx, tx, now)
}

// refreshOwnLocksBelowHalfTTLTx extends every lock owner currently holds
// whose remaining TTL has dropped under half of domain.DegradedModeTTL,
// inside the caller's own transaction — RecordCallPre's, so `loto hook pre`
// gets the heartbeat DESIGN.md:223 anticipated at no extra round trip
// (loto-wkul).
//
// One UPDATE ... RETURNING both selects the eligible rows and extends them —
// the "one UPDATE, no second round trip" the bead's Givens ask for. The
// WHERE clause carries the two guarantees down to SQL rather than trusting a
// caller's filter: owner_uuid = owner means a lock this owner does not hold
// is never touched (a peer's row, however close to expiry, is invisible to
// this query), and expires_at > now excludes an already-lapsed lease — that
// territory is reclaimable, and resurrecting it here would race the peer
// already entitled to take it (mirrors ErrLeaseExpired's reasoning in
// RefreshLocks/commitRefreshes, the manual verb's sibling).
//
// domain.DegradedModeTTL, not each row's own (expires_at - created_at): a
// refreshed row's span from created_at grows with every refresh, so reusing
// it as "the TTL" would make the half-TTL threshold drift upward forever.
// DegradedModeTTL is the fixed lease length both `loto lock` and `loto
// refresh` default to and the one the pre-hook's own admission
// (hookTakeLock) already takes locks under, so it is the one stable
// reference a caller who never set a custom --ttl is actually living under.
//
// Returns the refreshed targets (nil when none needed it) so the caller can
// decide whether to pay for rotateEventsTx — most hook calls touch nothing
// here and should not.
func refreshOwnLocksBelowHalfTTLTx(ctx context.Context, tx *sql.Tx, owner string, now time.Time) ([]domain.Target, error) {
	const ttl = domain.DegradedModeTTL
	newExpiry := now.Add(ttl)
	halfDeadline := now.Add(ttl / 2)

	// No acquireOpFlock here, unlike RefreshLocks/commitRefreshes: this
	// mutator only ever runs inside a caller-supplied tx (RecordCallPre's)
	// that is already a single immediate-mode write and touches no
	// filesystem — no chmod, no lock-file restore, nothing the flock exists
	// to serialize against a concurrent mode change. SQLite's own tx
	// isolation is what makes the owner_uuid-scoped UPDATE below safe without
	// it.
	rows, err := tx.QueryContext(ctx, `
UPDATE locks SET expires_at = ?
WHERE owner_uuid = ? AND expires_at > ? AND expires_at < ?
RETURNING target_canonical`,
		newExpiry.UnixNano(), owner, now.UnixNano(), halfDeadline.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []domain.Target
	for rows.Next() {
		var canon string
		if err := rows.Scan(&canon); err != nil {
			return nil, err
		}
		targets = append(targets, domain.Target{Canonical: canon})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}

	events := make([]domain.Event, len(targets))
	for i, t := range targets {
		events[i] = domain.Event{
			Target:    t,
			Kind:      EventLockRefreshed,
			ActorUUID: owner,
			Reason:    hookRefreshReason,
			CreatedAt: now,
		}
	}
	if err := appendEventsTx(ctx, tx, events); err != nil {
		return nil, err
	}
	return targets, nil
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

	k := s.keys()
	placeholders, canonArgs := k.inTargets(extend)
	args := append([]any{newExpiry.UnixNano(), owner, now.UnixNano()}, canonArgs...)
	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET expires_at = ? WHERE owner_uuid = ? AND expires_at > ? AND `+k.col("target_canonical")+` IN (`+placeholders+`)`, //nolint:gosec // G202 placeholders are '?' chars only, all data via args
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
