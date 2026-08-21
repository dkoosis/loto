package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestReleaseLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	bobRes, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks(bob): %v", err)
	}
	if bobRes[0].State != StateNotOwner {
		t.Fatalf("non-owner release must report StateNotOwner, got %+v", bobRes)
	}
	aliceRes, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks(alice): %v", err)
	}
	if aliceRes[0].State != StateUnlocked {
		t.Fatalf("owner release must report StateUnlocked, got %+v", aliceRes)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("lock should be gone, got %+v", got)
	}
}

func TestBreakLockStaleOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, tcTest, liveProbe, nil)
	if err != nil || res[0].Err == nil {
		t.Fatal("liveProbe break without force must fail")
	}
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakForce, "deadline", liveProbe, nil)
	if err != nil || res[0].Err != nil {
		t.Fatalf("force break: %v / %v", err, res[0].Err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("lock should be gone, got %+v", got)
	}
	events, _ := s.EventsForTarget(ctx, l.Target)
	var broken int
	for _, e := range events {
		if e.Kind == EventLockBroken {
			broken++
		}
	}
	if broken != 1 {
		t.Fatalf("expected exactly 1 lock_broken event, got %d in %+v", broken, events)
	}
}

// TestBreakLockStaleOnly_CrossHost verifies that BreakStale on a lock held by a
// remote host does NOT attempt pid-probing (which would be meaningless). The
// host comparison now lives inside the HolderLiveProbe closure (loto-ygty): a
// probe evaluating from "local-host" answers UNKNOWN for a holder stamped
// "remote-host", so TTL is the sole staleness authority there.
func TestBreakLockStaleOnly_CrossHost(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// Lock held on "remote-host" with a live pid probe that always returns true
	// (pid is "alive" on its host). The lock is NOT expired.
	l := mkFileLock(t, "cross.go", tcAlice, time.Hour)
	l.Host = "remote-host"
	l.PID = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, aliveOn("remote-host")); err != nil {
		t.Fatal(err)
	}

	// BreakStale from "local-host": the lock is on a different host, not
	// expired → the probe answers UNKNOWN, IsStale is false (can't probe a
	// remote pid), so the break must be refused.
	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "cross-host", aliveOn("local-host"), nil)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("BreakStale from different host on non-expired lock must fail")
	}

	// Same lock, but BreakStale from "remote-host" (same host as lock holder):
	// pid probe says alive → also refused.
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "same-host", aliveOn("remote-host"), nil)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("BreakStale from same host with live pid must fail")
	}

	// Same host, but pid probe says dead → stale, break succeeds.
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "same-host-dead", deadOn("remote-host"), nil)
	if err != nil || res[0].Err != nil {
		t.Fatalf("BreakStale from same host with dead pid should succeed: %v / %v", err, res[0].Err)
	}
}

// mustOpenWithRepoTop opens a store whose repo root is top. Use when a test
// needs the store to reconcile absolute candidates against repo-relative rows
// (loto-6e02: the root is a property of the store, not of the call).
func mustOpenWithRepoTop(t *testing.T, top string) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenContext(context.Background(), filepath.Join(dir, "loto.db"), WithRepoTop(top))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustOpen(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mkFileLock creates a realPath file under t.TempDir() and returns a LockRecord
// pointing to its absolute path with Kind=KindFile. Use when AcquireLocks
// validation requires the target to actually exist on disk.
func mkFileLock(t *testing.T, name, agent string, expIn time.Duration) domain.LockRecord {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return domain.LockRecord{
		Target:      domain.Target{Canonical: p},
		OwnerUUID:   domain.AgentUUID(agent),
		SessionUUID: domain.SessionUUID(agent),
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(expIn),
		Host:        "h",
		PID:         1,
	}
}

func TestBreakLocks_BatchedMultiTarget(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lb := mkFileLock(t, "b.go", tcAlice, time.Hour)
	lc := mkFileLock(t, "c.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb, lc}, liveProbe); err != nil {
		t.Fatal(err)
	}

	targets := []domain.Target{la.Target, lb.Target, lc.Target}
	results, err := s.BreakLocks(ctx, targets, tcBob, BreakForce, "batch break", liveProbe, nil)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d] err=%v", i, r.Err)
		}
		if r.Target != targets[i] {
			t.Errorf("results[%d] target=%v want %v (input order required)", i, r.Target, targets[i])
		}
		if got, _ := s.LockAt(ctx, targets[i]); got != nil {
			t.Errorf("targets[%d] should be gone, got %+v", i, got)
		}
		evs, _ := s.EventsForTarget(ctx, targets[i])
		var broken int
		for _, e := range evs {
			if e.Kind == EventLockBroken {
				broken++
			}
		}
		if broken != 1 {
			t.Errorf("targets[%d] expected 1 lock_broken event, got %d in %+v", i, broken, evs)
		}
	}
}

func TestBreakLocks_MixedNoLockAndOwned(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la}, liveProbe); err != nil {
		t.Fatal(err)
	}
	missing := domain.Target{Canonical: filepath.Join(t.TempDir(), "missing.go")}
	if err := os.WriteFile(missing.Canonical, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := s.BreakLocks(ctx, []domain.Target{la.Target, missing}, tcBob, BreakForce, "mixed", liveProbe, nil)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("owned target: %v", results[0].Err)
	}
	if !errors.Is(results[1].Err, ErrNoLockAtTarget) {
		t.Errorf("missing target: want ErrNoLockAtTarget, got %v", results[1].Err)
	}
}

func TestReleaseLocks_BatchedMixedStates(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lb := mkFileLock(t, "b.go", tcBob, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb}, liveProbe); err != nil {
		t.Fatal(err)
	}
	never := domain.Target{Canonical: filepath.Join(t.TempDir(), "never.go")}
	if err := os.WriteFile(never.Canonical, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{la.Target, lb.Target, never}, tcAlice, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	if res[0].State != StateUnlocked {
		t.Errorf("res[0]: want StateUnlocked, got %v", res[0].State)
	}
	if res[1].State != StateNotOwner || res[1].Owner != tcBob {
		t.Errorf("res[1]: want StateNotOwner owner=bob, got state=%v owner=%v", res[1].State, res[1].Owner)
	}
	if res[2].State != StateNoLock {
		t.Errorf("res[2]: want StateNoLock, got %v", res[2].State)
	}
}

func TestAcquireOverlapBlocks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	aliceLock := domain.LockRecord{
		Target:      domain.Target{Canonical: a},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{aliceLock}, liveProbe); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	bobLock := aliceLock
	bobLock.OwnerUUID = tcBob
	bobLock.SessionUUID = tcBob
	res, err := s.AcquireLocks(ctx, []domain.LockRecord{bobLock}, liveProbe)
	if err == nil {
		t.Fatalf("expected conflict; got result %+v", res)
	}
	var conflict *MultiConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *MultiConflictError; got %T", err)
	}
	if len(conflict.Blockers) != 1 || conflict.Blockers[0].OwnerUUID != tcAlice {
		t.Fatalf("expected single blocker alice; got %+v", conflict.Blockers)
	}
}

// TestAcquireNoDurablePidBlocksUntilTTL is the core loto-j1bo acceptance: a
// lock placed without a durable pid (PID 0 sentinel, LOTO_PID unset) must be
// treated as a LIVE blocker until its TTL — even under a dead liveness probe — so
// a peer cannot silently reclaim it. Contrast TestAcquireOverlapBlocks (real pid,
// live probe) and the reclaim tests (real pid, dead probe → reclaimed).
func TestAcquireNoDurablePidBlocksUntilTTL(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	aliceLock := domain.LockRecord{
		Target:      domain.Target{Canonical: a},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         0, // no durable pid → TTL governs
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{aliceLock}, deadProbe); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	bobLock := aliceLock
	bobLock.OwnerUUID = tcBob
	bobLock.SessionUUID = tcBob
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{bobLock}, deadProbe)
	var conflict *MultiConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("PID-0 lock within TTL must block a peer (not reclaim); got err=%v", err)
	}
	if len(conflict.Blockers) != 1 || conflict.Blockers[0].OwnerUUID != tcAlice {
		t.Fatalf("expected single blocker alice; got %+v", conflict.Blockers)
	}
}

func TestAcquireSameAgentRefreshes(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()

	first := domain.LockRecord{
		Target:      domain.Target{Canonical: a},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		Intent:      "first",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{first}, liveProbe); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Intent = "refreshed"
	second.ExpiresAt = first.ExpiresAt.Add(time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{second}, liveProbe); err != nil {
		t.Fatalf("refresh must succeed: %v", err)
	}
	got, err := s.LockAt(ctx, second.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Intent != "refreshed" {
		t.Fatalf("refresh did not update; got %+v", got)
	}
}

func TestAcquireLocks_MultiFile_AtomicSuccess(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   domain.AgentUUID(owner),
			SessionUUID: domain.SessionUUID(owner),
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}
	recs := []domain.LockRecord{mk(a, tcAlice), mk(b, tcAlice)}

	if _, err := s.AcquireLocks(ctx, recs, liveProbe); err != nil {
		t.Fatalf("AcquireLocks: %v", err)
	}

	// loto-zssw: the rows are the whole effect — no mode bits move.
	for _, p := range []string{a, b} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s: acquire must not touch mode bits, got %o", p, st.Mode().Perm())
		}
	}
}

func TestAcquireLocks_MultiFile_ConflictAbortsAtomically(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   domain.AgentUUID(owner),
			SessionUUID: domain.SessionUUID(owner),
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}

	// alice already holds a.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a, tcAlice)}, liveProbe); err != nil {
		t.Fatal(err)
	}
	stA, _ := os.Stat(a)
	modeABefore := stA.Mode().Perm()
	stB, _ := os.Stat(b)
	modeBBefore := stB.Mode().Perm()

	// bob tries to acquire both. Should fail, no chmod side effect on b.
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a, tcBob), mk(b, tcBob)}, liveProbe)
	if err == nil {
		t.Fatal("expected conflict, got nil")
	}
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("want *MultiConflictError, got %T", err)
	}

	stA2, _ := os.Stat(a)
	if stA2.Mode().Perm() != modeABefore {
		t.Errorf("a mode changed: %o -> %o", modeABefore, stA2.Mode().Perm())
	}
	stB2, _ := os.Stat(b)
	if stB2.Mode().Perm() != modeBBefore {
		t.Errorf("b mode changed: %o -> %o (should be untouched)", modeBBefore, stB2.Mode().Perm())
	}
}

// Regression for gh#122: post-commit restore-failure audit must land even
// when the caller's ctx is already cancelled. Pre-fix, AppendEvents
// opened a fresh tx under the cancelled ctx → busy_timeout scaled to ~1ms
// → audit silently dropped, leaving orphan-mode files with zero trail.
func TestReleaseLocks_NoLockVsNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	l := mkFileLock(t, "x.go", tcAlice, time.Hour)
	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks (no row): %v", err)
	}
	if res[0].State != StateNoLock {
		t.Fatalf("expected StateNoLock, got %+v", res)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	res, err = s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks (not owner): %v", err)
	}
	if res[0].State != StateNotOwner {
		t.Fatalf("expected StateNotOwner, got %+v", res)
	}
}

func TestReleaseLocks_DistinguishesMissingFromNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	c := mkFileLock(t, "c.go", tcBob, time.Hour)
	bDir := t.TempDir()
	bPath := filepath.Join(bDir, "b.go")
	if err := os.WriteFile(bPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{c}, liveProbe); err != nil {
		t.Fatal(err)
	}

	results, err := s.ReleaseLocks(ctx, []domain.Target{
		a.Target,
		{Canonical: bPath},
		c.Target,
	}, tcAlice, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	want := []ReleaseOutcome{StateUnlocked, StateNoLock, StateNotOwner}
	for i, r := range results {
		if r.State != want[i] {
			t.Errorf("results[%d].State = %v, want %v", i, r.State, want[i])
		}
	}
	if results[2].Owner != tcBob {
		t.Errorf("results[2].Owner = %q, want %q", results[2].Owner, tcBob)
	}
	for _, l := range []domain.LockRecord{a, c} {
		st, _ := os.Stat(l.Target.Canonical)
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s: release must not touch mode bits, got %o", l.Target.Canonical, st.Mode().Perm())
		}
	}
}

// mkTargetRec wraps a bare canonical path into the non-beacon LockRecord
// validateFileTarget now takes — plain `loto lock` shape, the case every
// TestValidateFileTarget_TypedErrors assertion predates the beacon carve-out
// with (loto-z5nb).
func mkTargetRec(canonical string) domain.LockRecord {
	return domain.LockRecord{Target: domain.Target{Canonical: canonical}}
}

// TestValidateFileTarget_TypedErrors verifies validateFileTarget returns
// *TargetValidationError with reason + Nlink preserved (replaces the prior
// bare ErrTarget* sentinels that lost state across the wrap).
func TestValidateFileTarget_TypedErrors(t *testing.T) {
	dir := t.TempDir()

	// symlink → ReasonSymlink
	realPath := filepath.Join(dir, "realPath.go")
	if err := os.WriteFile(realPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(dir, "sym.go")
	if err := os.Symlink(realPath, sym); err != nil {
		t.Fatal(err)
	}
	var tve *TargetValidationError
	if err := validateFileTarget("", mkTargetRec(sym)); !errors.As(err, &tve) || tve.Reason != ReasonSymlink {
		t.Fatalf("symlink: got %v, want ReasonSymlink", err)
	}

	// directory → ReasonNotRegular
	tve = nil
	if err := validateFileTarget("", mkTargetRec(dir)); !errors.As(err, &tve) || tve.Reason != ReasonNotRegular {
		t.Fatalf("dir: got %v, want ReasonNotRegular", err)
	}

	// hard link → ReasonMultiLinked + Nlink populated
	hard := filepath.Join(dir, "hard.go")
	if err := os.Link(realPath, hard); err != nil {
		t.Fatal(err)
	}
	tve = nil
	if err := validateFileTarget("", mkTargetRec(realPath)); !errors.As(err, &tve) || tve.Reason != ReasonMultiLinked {
		t.Fatalf("multi-link: got %v, want ReasonMultiLinked", err)
	}
	if tve.Nlink < 2 {
		t.Fatalf("Nlink not preserved: got %d, want >= 2", tve.Nlink)
	}
}

// TestValidateFileTarget_BeaconToleratesMissingPath is loto-z5nb's own AC: a
// beacon for a path that does not exist yet must pass, since it announces a
// Write about to CREATE it — the case AcquireLocks previously had nothing to
// protect.
func TestValidateFileTarget_BeaconToleratesMissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "new.go")

	beacon := domain.LockRecord{Target: domain.Target{Canonical: missing}, Mode: domain.ModeShared, PID: 0, Beacon: true}
	if !beacon.IsBeacon() {
		t.Fatal("test fixture invariant broken: want a beacon shape")
	}
	if err := validateFileTarget("", beacon); err != nil {
		t.Errorf("beacon on a missing path must be allowed, got %v", err)
	}

	// The carve-out is ENOENT-only: a beacon whose path IS a symlink is still
	// refused, same as a plain lock.
	realPath := filepath.Join(dir, "real.go")
	if err := os.WriteFile(realPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(dir, "sym.go")
	if err := os.Symlink(realPath, sym); err != nil {
		t.Fatal(err)
	}
	beaconOnSymlink := domain.LockRecord{Target: domain.Target{Canonical: sym}, Mode: domain.ModeShared, PID: 0, Beacon: true}
	var tve *TargetValidationError
	if err := validateFileTarget("", beaconOnSymlink); !errors.As(err, &tve) || tve.Reason != ReasonSymlink {
		t.Fatalf("beacon on an existing symlink must still be refused, got %v", err)
	}

	// A plain (non-beacon) lock on a missing path is unaffected — the carve-out
	// is beacon-scoped, not a general relaxation.
	plain := mkTargetRec(missing)
	if err := validateFileTarget("", plain); err == nil {
		t.Error("a plain lock on a missing path must still be refused")
	}
}

// TestAcquireReclaimsDeadSession pins loto-k5el.1 SC2 (dead half): a holder whose
// session pid is provably dead is reclaimed on a peer's acquire, within TTL.
//
// Not TDD — the IsStale+injected-probe mechanism already ships; this test passes
// on first write and pins SC2 against regression.
//
// Harness note (Task 0): there is no openTestStore/mustInsertLock. We use the
// real helpers mustOpen + mkFileLock (which creates the on-disk target
// AcquireLocks Lstat-validates) and seed the holder via a first AcquireLocks
// with a live probe. The holder carries a durable pid (PID>0, ProcStart set) and
// Host "h" — matching the acquirer's host so reclaimStaleAndCollectBlockers
// probes it. The peer's acquire then drives IsStale through a dead probe.
func TestAcquireReclaimsDeadSession(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	dead := mkFileLock(t, "a.go", tcAlice, time.Hour) // TTL NOT expired
	dead.PID = 4242                                   // durable pid (probe will say dead)
	dead.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{dead}, liveProbe); err != nil {
		t.Fatalf("seed alice lock: %v", err)
	}

	// Probe reports pid 4242 dead → liveness-primary reclaim despite liveProbe TTL.
	bob := dead
	bob.OwnerUUID, bob.SessionUUID, bob.PID = tcBob, tcBob, 5555
	got, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, deadProbe)
	if err != nil {
		t.Fatalf("bob acquire over dead-session holder must succeed: %v", err)
	}
	if len(got) != 1 || got[0].OwnerUUID != tcBob {
		t.Fatalf("expected bob to hold the reclaimed lock, got %+v", got)
	}
	held, _ := s.LockAt(ctx, dead.Target)
	if held == nil || held.OwnerUUID != tcBob {
		t.Fatalf("store should show bob as holder after reclaim, got %+v", held)
	}
}

// TestAcquireBlocksOnLiveSession pins loto-k5el.1 SC2 (live half): a holder whose
// session pid is alive and TTL unexpired is NOT reclaimed — peer acquire conflicts.
func TestAcquireBlocksOnLiveSession(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	held := mkFileLock(t, "a.go", tcAlice, time.Hour)
	held.PID = 4242
	held.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held}, liveProbe); err != nil {
		t.Fatalf("seed alice lock: %v", err)
	}

	bob := held
	bob.OwnerUUID, bob.SessionUUID, bob.PID = tcBob, tcBob, 5555
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe)
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("bob acquire over LIVE holder must conflict, got err=%v", err)
	}
}

// TestValidateFileTarget_RepoRelativeCanonicalJoinsRepoTop is the loto-3tv3 D8
// regression. Canonicals are repo-relative; probing one bare resolves it
// against the process CWD, so from any cwd but the repo root the store either
// misses the file or stats a same-named neighbour.
func TestValidateFileTarget_RepoRelativeCanonicalJoinsRepoTop(t *testing.T) {
	repoTop := t.TempDir()
	sub := filepath.Join(repoTop, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // a cwd that is not the repo top

	if err := validateFileTarget(repoTop, mkTargetRec("sub/a.go")); err != nil {
		t.Errorf("with repoTop: %v", err)
	}
	if err := validateFileTarget("", mkTargetRec("sub/a.go")); err == nil {
		t.Error("without repoTop the bare probe must miss — that is the defect being pinned")
	}
}

// TestValidateFileTarget_EmptyRepoTopLegacyUnchanged is why ~40 mkFileLock call
// sites needed no edit: an absolute canonical with no repoTop probes exactly
// what it probed before.
func TestValidateFileTarget_EmptyRepoTopLegacyUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateFileTarget("", mkTargetRec(p)); err != nil {
		t.Errorf("absolute canonical, no repoTop: %v", err)
	}
}

// TestValidateFileTarget_AbsoluteCanonicalIgnoresRepoTop pins that the join is
// conditional: joining repoTop to an absolute path would produce nonsense.
func TestValidateFileTarget_AbsoluteCanonicalIgnoresRepoTop(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateFileTarget(t.TempDir(), mkTargetRec(p)); err != nil {
		t.Errorf("absolute canonical with a repoTop set: %v", err)
	}
}

// TestValidateFileTarget_BeaconMissingLeafStillTolerated keeps the loto-z5nb
// carve-out intact under the join: a beacon announces a write to a path that
// may not exist yet.
func TestValidateFileTarget_BeaconMissingLeafStillTolerated(t *testing.T) {
	repoTop := t.TempDir()
	t.Chdir(t.TempDir())
	rec := mkTargetRec("sub/brand-new.go")
	rec.Beacon = true
	if err := validateFileTarget(repoTop, rec); err != nil {
		t.Errorf("beacon on a missing path: %v", err)
	}
}

// TestAcquireLocks_WithRepoTopFromForeignCwd is the end-to-end "correct from
// any CWD": a store opened with WithRepoTop accepts a repo-relative canonical
// no matter where the process is standing.
func TestAcquireLocks_WithRepoTopFromForeignCwd(t *testing.T) {
	ctx := context.Background()
	repoTop := t.TempDir()
	sub := filepath.Join(repoTop, "internal", "store")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "locks.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenContext(ctx, filepath.Join(t.TempDir(), "loto.db"), WithRepoTop(repoTop))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	t.Chdir(t.TempDir())

	rec := mkTargetRec("internal/store/locks.go")
	rec.OwnerUUID = tcAlice
	rec.SessionUUID = tcAlice
	rec.ExpiresAt = time.Now().Add(time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
		t.Fatalf("acquire from a foreign cwd: %v", err)
	}
}
