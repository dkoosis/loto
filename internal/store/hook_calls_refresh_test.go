package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// tcHookRefreshTTL is the fixed lease length refreshOwnLocksBelowHalfTTLTx
// measures against (domain.DegradedModeTTL), named here so every test below
// reads its arithmetic without re-deriving the constant.
const tcHookRefreshTTL = domain.DegradedModeTTL

// preAt is preAs (tree_events_test.go) with no observed paths — the refresh
// runs on every pre regardless of which file the call touches, per the
// bead's Rules ("on every loto hook pre").
func preAt(t *testing.T, s *Store, owner domain.AgentUUID, callID string, at time.Time) {
	t.Helper()
	preAs(t, s, owner, callID, at)
}

// TestRecordCallPre_RefreshesOwnLockUnderHalfTTL is loto-wkul's first AC: a
// lane holding a lock, driven by synthetic pre hooks past the point where
// ttl_remaining < ttl/2, still owns the lock after the ORIGINAL expiry, with
// no `loto refresh` call anywhere in the test.
func TestRecordCallPre_RefreshesOwnLockUnderHalfTTL(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL) // expires t0+30m
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// t0+16m: remaining is 14m, under half of 30m — the hook must refresh.
	preAt(t, s, tcOwnerA, "call-1", t0.Add(16*time.Minute))

	after, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || after == nil {
		t.Fatalf("read back: %v %v", after, err)
	}
	if !after.ExpiresAt.After(t0.Add(tcHookRefreshTTL)) {
		t.Fatalf("want expires_at pushed past the original deadline %v, got %v", t0.Add(tcHookRefreshTTL), after.ExpiresAt)
	}
	if !after.CreatedAt.Equal(t0) {
		t.Errorf("refresh must not restart the hold: created_at moved to %v", after.CreatedAt)
	}

	// t0+31m: past the ORIGINAL expiry. The lock is still held, live, because
	// the hook refreshed it at t0+16m without any manual `loto refresh`.
	stillHeld, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || stillHeld == nil {
		t.Fatalf("read back at original-expiry+1m: %v %v", stillHeld, err)
	}
	if !stillHeld.ExpiresAt.After(t0.Add(31 * time.Minute)) {
		t.Errorf("lock reads as expired past its original TTL: expires_at=%v", stillHeld.ExpiresAt)
	}
}

// TestRecordCallPre_LeavesALockAboveHalfTTLAlone keeps the common case cheap
// and honest: a lock with plenty of remaining TTL is not touched, so the
// UPDATE ... RETURNING in refreshOwnLocksBelowHalfTTLTx affects zero rows on
// the hot path.
func TestRecordCallPre_LeavesALockAboveHalfTTLAlone(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// t0+5m: remaining is 25m, well above half of 30m — no refresh.
	preAt(t, s, tcOwnerA, "call-1", t0.Add(5*time.Minute))

	after, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || after == nil {
		t.Fatalf("read back: %v %v", after, err)
	}
	if !after.ExpiresAt.Equal(t0.Add(tcHookRefreshTTL)) {
		t.Errorf("lock was touched above half TTL: expires_at=%v want %v", after.ExpiresAt, t0.Add(tcHookRefreshTTL))
	}
	evs, err := s.EventsForTarget(ctx, rec.Target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if e.Kind == EventLockRefreshed {
			t.Errorf("unwanted %s event above half TTL: %+v", EventLockRefreshed, e)
		}
	}
}

// TestRecordCallPre_NeverRefreshesAPeersLock is loto-wkul's third AC: B's pre
// never refreshes A's lock, however close to expiry A's lock is.
func TestRecordCallPre_NeverRefreshesAPeersLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	before, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || before == nil {
		t.Fatalf("read back before: %v %v", before, err)
	}

	// B's own pre, well under A's lock's half-TTL point, must not touch it —
	// B holds nothing at this target, so B's owner_uuid excludes A's row by
	// construction.
	preAt(t, s, tcOwnerB, "call-b1", t0.Add(16*time.Minute))

	after, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || after == nil {
		t.Fatalf("read back after: %v %v", after, err)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("B's pre moved A's lease: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}
	evs, err := s.EventsForTarget(ctx, rec.Target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if e.Kind == EventLockRefreshed {
			t.Errorf("B's pre wrote a refresh event on A's lock: %+v", e)
		}
	}
}

// TestRecordCallPre_RefreshEventCarriesReasonHook is loto-wkul's fourth AC:
// `loto events --kind lock_refreshed` shows one row per hook refresh, and
// each carries reason=hook — distinct from the manual verb's own
// "ttl extended to <ts>" reason (commitRefreshes).
func TestRecordCallPre_RefreshEventCarriesReasonHook(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	preAt(t, s, tcOwnerA, "call-1", t0.Add(16*time.Minute))

	evs, err := s.EventsForTarget(ctx, rec.Target)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var found int
	for _, e := range evs {
		if e.Kind != EventLockRefreshed {
			continue
		}
		found++
		if e.Reason != "hook" {
			t.Errorf("want reason=hook, got %q", e.Reason)
		}
		if e.ActorUUID != string(tcOwnerA) {
			t.Errorf("want actor=%s, got %s", tcOwnerA, e.ActorUUID)
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one %s event, got %d in %+v", EventLockRefreshed, found, evs)
	}
}

// TestRecordCallPre_NeverResurrectsAnAlreadyExpiredLease keeps the reclaim
// path intact (loto-wkul's second AC, at the boundary this code actually
// touches): a lease already past its deadline is territory the existing
// stale/dead-owner path may reclaim, and the hook refresh must not extend it
// back to life just because its own owner happened to call a hook again.
func TestRecordCallPre_NeverResurrectsAnAlreadyExpiredLease(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, -time.Minute) // already lapsed
	rec.CreatedAt, rec.ExpiresAt = t0.Add(-2*time.Hour), t0.Add(-time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	preAt(t, s, tcOwnerA, "call-1", t0)

	after, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || after == nil {
		t.Fatalf("read back: %v %v", after, err)
	}
	if after.ExpiresAt.After(t0) {
		t.Errorf("expired lease was resurrected by the hook: expires_at=%v", after.ExpiresAt)
	}
}
