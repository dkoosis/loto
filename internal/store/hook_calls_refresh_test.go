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

// preAt is preAs (tree_events_test.go) with the given observed paths. The
// refresh renews only a lock on a path the call declares (loto-hous), so a
// test that wants a refresh passes touching(rec).
func preAt(t *testing.T, s *Store, owner domain.AgentUUID, callID string, at time.Time, obs ...HookPathState) {
	t.Helper()
	preAs(t, s, owner, callID, at, obs...)
}

// touching is the observation an Edit-family call on rec's file carries: the
// path it was admitted on, locked, declared.
func touching(rec domain.LockRecord) HookPathState {
	return HookPathState{Path: rec.Target.Canonical, Locked: true, Declared: true, Epoch: 1, Holder: rec.OwnerUUID, Digest: tcSHA1}
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
	preAt(t, s, tcOwnerA, "call-1", t0.Add(16*time.Minute), touching(rec))

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

	// A further pre at t0+31m — past the ORIGINAL 30m expiry — finds the lock
	// already refreshed to t0+46m: still well above ITS half-TTL point, so
	// this call writes no second refresh. This is the bead's first AC in
	// full: the lane crosses its original deadline alive, with no manual
	// `loto refresh` anywhere in the test.
	preAt(t, s, tcOwnerA, "call-2", t0.Add(31*time.Minute), touching(rec))

	stillHeld, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || stillHeld == nil {
		t.Fatalf("read back at t0+31m: %v %v", stillHeld, err)
	}
	if stillHeld.ExpiresAt.Before(t0.Add(31 * time.Minute)) {
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
	preAt(t, s, tcOwnerA, "call-1", t0.Add(5*time.Minute), touching(rec))

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
	preAt(t, s, tcOwnerB, "call-b1", t0.Add(16*time.Minute), touching(rec))

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

	preAt(t, s, tcOwnerA, "call-1", t0.Add(16*time.Minute), touching(rec))

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

	preAt(t, s, tcOwnerA, "call-1", t0, touching(rec))

	after, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || after == nil {
		t.Fatalf("read back: %v %v", after, err)
	}
	if after.ExpiresAt.After(t0) {
		t.Errorf("expired lease was resurrected by the hook: expires_at=%v", after.ExpiresAt)
	}
}

// TestRotateEvents_LockRefreshedHasItsOwnCap mirrors
// TestRotateEvents_HookTimingHasItsOwnCap: lock_refreshed writes one row per
// held lock roughly every half-TTL for as long as a lane keeps calling the
// hook, and without its own cap a long session would evict every lock, gate
// and admission event under the shared 1000-row ceiling the same way
// hook_timing did before HookTimingRetentionMax existed.
func TestRotateEvents_LockRefreshedHasItsOwnCap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	// One low-rate row of another kind, written first so it is the OLDEST and
	// therefore the first casualty of a global-cap-only rotation.
	if _, err := s.AppendEvent(ctx, domain.Event{
		Kind: EventGateBypass, ActorUUID: tcOwnerA, Reason: "seed", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	for i := range LockRefreshedRetentionMax + 50 {
		if _, err := s.AppendEventRotating(ctx, domain.Event{
			Kind: EventLockRefreshed, ActorUUID: tcOwnerA, Reason: "hook",
			CreatedAt: now.Add(time.Duration(i+1) * time.Millisecond),
		}); err != nil {
			t.Fatalf("refreshed row %d: %v", i, err)
		}
	}

	var refreshed, bypass int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = ?`, EventLockRefreshed).Scan(&refreshed); err != nil {
		t.Fatalf("count refreshed: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = ?`, EventGateBypass).Scan(&bypass); err != nil {
		t.Fatalf("count bypass: %v", err)
	}
	if refreshed != LockRefreshedRetentionMax {
		t.Errorf("lock_refreshed rows = %d, want its cap of %d", refreshed, LockRefreshedRetentionMax)
	}
	if bypass != 1 {
		t.Errorf("the hook evicted an unrelated kind: gate_bypass rows = %d, want 1", bypass)
	}
}

// TestRecordCallPre_RefreshScopedToCallWorktree (PR #373 review): one owner
// holding a.go in worktrees A and B, with hook activity only in A, must not
// keep B's lease alive — otherwise a dead B session's lock never goes stale.
func TestRecordCallPre_RefreshScopedToCallWorktree(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()
	const wtA, wtB = "/repo/wtA", "/repo/wtB"

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	for _, wt := range []string{wtA, wtB} {
		r := rec
		r.Worktree = wt
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{r}, liveProbe); err != nil {
			t.Fatalf("acquire %s: %v", wt, err)
		}
	}

	preAsIn(t, s, tcOwnerA, "call-in-A", wtA, t0.Add(16*time.Minute), touching(rec))

	rows, err := s.LocksAt(ctx, rec.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want both worktrees' rows, got %+v", rows)
	}
	for _, r := range rows {
		refreshed := r.ExpiresAt.After(t0.Add(tcHookRefreshTTL))
		if r.Worktree == wtA && !refreshed {
			t.Errorf("A's own lease must be refreshed, expires_at=%v", r.ExpiresAt)
		}
		if r.Worktree == wtB && refreshed {
			t.Errorf("hook activity in A must not refresh B's lease, expires_at=%v", r.ExpiresAt)
		}
	}
}

// TestRecordCallPre_LeavesAnUntouchedLockToLapse is loto-hous's first AC: an
// owner working only in b.go does not keep its lock on a.go alive. dk
// (2026-09-28): an agent's time in one file is short, and a zombie lock is
// the common failure — a lock the holder stopped touching lapses on its own
// lease, however busy the holder is elsewhere.
func TestRecordCallPre_LeavesAnUntouchedLockToLapse(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	recA := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	recA.CreatedAt, recA.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	recB := mkFileLock(t, "b.go", tcOwnerA, tcHookRefreshTTL)
	recB.CreatedAt, recB.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{recA, recB}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// t0+16m: both under half TTL; the call edits b.go only.
	preAt(t, s, tcOwnerA, "call-b", t0.Add(16*time.Minute), touching(recB))

	a, err := s.LockForOwnerAt(ctx, recA.Target, tcOwnerA)
	if err != nil || a == nil {
		t.Fatalf("read back a.go: %v %v", a, err)
	}
	if !a.ExpiresAt.Equal(t0.Add(tcHookRefreshTTL)) {
		t.Errorf("work on b.go renewed the untouched a.go: expires_at=%v want %v", a.ExpiresAt, t0.Add(tcHookRefreshTTL))
	}
	b, err := s.LockForOwnerAt(ctx, recB.Target, tcOwnerA)
	if err != nil || b == nil {
		t.Fatalf("read back b.go: %v %v", b, err)
	}
	if !b.ExpiresAt.After(t0.Add(tcHookRefreshTTL)) {
		t.Errorf("the touched b.go was not renewed: expires_at=%v", b.ExpiresAt)
	}
}

// TestRecordCallPost_RenewsALockWhoseFileTheCallChanged is loto-hous's second
// AC for writes no Edit declares: a Bash call that rewrites a locked file (a
// sed, a formatter) renews the caller's lock on it at post.
func TestRecordCallPost_RenewsALockWhoseFileTheCallChanged(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	t0 := time.Now()

	rec := mkFileLock(t, "a.go", tcOwnerA, tcHookRefreshTTL)
	rec.CreatedAt, rec.ExpiresAt = t0, t0.Add(tcHookRefreshTTL)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	pre := touching(rec)
	pre.Declared = false // a Bash call names no file
	preAt(t, s, tcOwnerA, "call-bash", t0.Add(16*time.Minute), pre)
	if a, _ := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA); a == nil || !a.ExpiresAt.Equal(t0.Add(tcHookRefreshTTL)) {
		t.Fatalf("an undeclared observation renewed the lock at pre: %+v", a)
	}

	post := pre
	post.Digest = tcSHA2
	postAt(t, s, "call-bash", t0.Add(17*time.Minute), post)

	a, err := s.LockForOwnerAt(ctx, rec.Target, tcOwnerA)
	if err != nil || a == nil {
		t.Fatalf("read back: %v %v", a, err)
	}
	if !a.ExpiresAt.After(t0.Add(tcHookRefreshTTL)) {
		t.Errorf("a call that changed the locked file did not renew its lock: expires_at=%v", a.ExpiresAt)
	}
}
