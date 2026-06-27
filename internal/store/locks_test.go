package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestReleaseLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	bobRes, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob)
	if err != nil {
		t.Fatalf("ReleaseLocks(bob): %v", err)
	}
	if bobRes[0].State != StateNotOwner {
		t.Fatalf("non-owner release must report StateNotOwner, got %+v", bobRes)
	}
	aliceRes, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice)
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
	live := func(string, int, int64) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, tcTest, "h", live)
	if err != nil || res[0].Err == nil {
		t.Fatal("live break without force must fail")
	}
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakForce, "deadline", "h", live)
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
// remote host does NOT attempt pid-probing (which would be meaningless). Before
// the fix, classifyBreaks passed l.Host as thisHost, making IsStale always see
// l.Host == thisHost → always pid-probe. With the fix, the requester's host is
// threaded through, so IsStale correctly skips the pid check for cross-host
// locks and falls back to TTL-only staleness.
func TestBreakLockStaleOnly_CrossHost(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// Lock held on "remote-host" with a live pid probe that always returns true
	// (pid is "alive" on its host). The lock is NOT expired.
	l := mkFileLock(t, "cross.go", tcAlice, time.Hour)
	l.Host = "remote-host"
	l.PID = 9999
	live := func(string, int, int64) bool { return true }
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	// BreakStale from "local-host": the lock is on a different host, not
	// expired → IsStale should return false (can't probe remote pid), so the
	// break must be refused.
	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "cross-host", "local-host", live)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("BreakStale from different host on non-expired lock must fail")
	}

	// Same lock, but BreakStale from "remote-host" (same host as lock holder):
	// pid probe says alive → also refused.
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "same-host", "remote-host", live)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("BreakStale from same host with live pid must fail")
	}

	// Same host, but pid probe says dead → stale, break succeeds.
	dead := func(string, int, int64) bool { return false }
	res, err = s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "same-host-dead", "remote-host", dead)
	if err != nil || res[0].Err != nil {
		t.Fatalf("BreakStale from same host with dead pid should succeed: %v / %v", err, res[0].Err)
	}
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
		SessionUUID: agent,
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(expIn),
		Host:        "h",
		PID:         1,
	}
}

func TestBreakLocks_RestoreErrSurfaced(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	// Inject chmod failure for the restore phase only (post-strip). The strip
	// during AcquireLocks already ran via the realPath fchmodFn; flip it now so the
	// post-commit restoreWrite returns EPERM.
	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == l.Target.Canonical {
			return &os.PathError{Op: tcChmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	results, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakForce, "restore-fail", "h", live)
	if err != nil {
		t.Fatalf("BreakLocks: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("break itself should succeed, got Err=%v", results[0].Err)
	}
	if results[0].RestoreErr == nil {
		t.Fatal("expected RestoreErr to surface chmod-restore failure")
	}
	// Audit event also emitted.
	evs, _ := s.EventsForTarget(ctx, l.Target)
	gotRestoreFailed := false
	for _, e := range evs {
		if e.Kind == EventModeRestoreFailed {
			gotRestoreFailed = true
		}
	}
	if !gotRestoreFailed {
		t.Errorf("expected mode_restore_failed event, got %+v", evs)
	}
}

func TestBreakLocks_BatchedMultiTarget(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lb := mkFileLock(t, "b.go", tcAlice, time.Hour)
	lc := mkFileLock(t, "c.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb, lc}, live); err != nil {
		t.Fatal(err)
	}

	targets := []domain.Target{la.Target, lb.Target, lc.Target}
	results, err := s.BreakLocks(ctx, targets, tcBob, BreakForce, "batch break", "h", live)
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
	live := func(string, int, int64) bool { return true }

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la}, live); err != nil {
		t.Fatal(err)
	}
	missing := domain.Target{Canonical: filepath.Join(t.TempDir(), "missing.go")}
	if err := os.WriteFile(missing.Canonical, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := s.BreakLocks(ctx, []domain.Target{la.Target, missing}, tcBob, BreakForce, "mixed", "h", live)
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
	live := func(string, int, int64) bool { return true }

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lb := mkFileLock(t, "b.go", tcBob, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb}, live); err != nil {
		t.Fatal(err)
	}
	never := domain.Target{Canonical: filepath.Join(t.TempDir(), "never.go")}
	if err := os.WriteFile(never.Canonical, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{la.Target, lb.Target, never}, tcAlice)
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
	live := func(string, int, int64) bool { return true }
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
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{aliceLock}, live); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	bobLock := aliceLock
	bobLock.OwnerUUID = tcBob
	bobLock.SessionUUID = tcBob
	res, err := s.AcquireLocks(ctx, []domain.LockRecord{bobLock}, live)
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
	dead := func(string, int, int64) bool { return false }
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
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{aliceLock}, dead); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	bobLock := aliceLock
	bobLock.OwnerUUID = tcBob
	bobLock.SessionUUID = tcBob
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{bobLock}, dead)
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
	live := func(string, int, int64) bool { return true }
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
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{first}, live); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Intent = "refreshed"
	second.ExpiresAt = first.ExpiresAt.Add(time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{second}, live); err != nil {
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
	live := func(string, int, int64) bool { return true }
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   domain.AgentUUID(owner),
			SessionUUID: owner,
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}
	recs := []domain.LockRecord{mk(a, tcAlice), mk(b, tcAlice)}

	if _, err := s.AcquireLocks(ctx, recs, live); err != nil {
		t.Fatalf("AcquireLocks: %v", err)
	}

	for _, p := range []string{a, b} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s: expected stripped write, got %o", p, st.Mode().Perm())
		}
	}
}

func TestAcquireLocks_MultiFile_ConflictAbortsNoChmod(t *testing.T) {
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
	live := func(string, int, int64) bool { return true }
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   domain.AgentUUID(owner),
			SessionUUID: owner,
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}

	// alice already holds a.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a, tcAlice)}, live); err != nil {
		t.Fatal(err)
	}
	stA, _ := os.Stat(a)
	modeABefore := stA.Mode().Perm()
	stB, _ := os.Stat(b)
	modeBBefore := stB.Mode().Perm()

	// bob tries to acquire both. Should fail, no chmod side effect on b.
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a, tcBob), mk(b, tcBob)}, live)
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

func TestAcquireLocks_ChmodFailureRollsBack(t *testing.T) {
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

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == b {
			return &os.PathError{Op: tcChmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	live := func(string, int, int64) bool { return true }
	now := time.Now()
	mk := func(p string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   tcAlice,
			SessionUUID: "s1",
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a), mk(b)}, live)
	var cfe *ChmodFailureError
	if !errors.As(err, &cfe) {
		t.Fatalf("want *ChmodFailureError, got %v", err)
	}

	stA, _ := os.Stat(a)
	if stA.Mode().Perm()&0o200 == 0 {
		t.Errorf("a not restored: %o", stA.Mode().Perm())
	}
	locks, _ := s.ListLocks(ctx)
	if len(locks) != 0 {
		t.Errorf("expected 0 locks, got %d", len(locks))
	}
}

func TestAcquireLocks_RollbackRestoreFailureLeavesBreadcrumb(t *testing.T) {
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

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		switch {
		case f.Name() == b:
			return &os.PathError{Op: tcChmod, Path: f.Name(), Err: syscall.EPERM}
		case f.Name() == a && mode.Perm()&0o200 != 0:
			return &os.PathError{Op: tcChmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	live := func(string, int, int64) bool { return true }
	now := time.Now()
	mk := func(p string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   tcAlice,
			SessionUUID: "s1",
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{mk(a), mk(b)}, live)
	var cfe *ChmodFailureError
	if !errors.As(err, &cfe) {
		t.Fatalf("want *ChmodFailureError, got %v", err)
	}

	var aFailure *ChmodFailure
	for i := range cfe.Failures {
		if cfe.Failures[i].Target.Canonical == a {
			aFailure = &cfe.Failures[i]
		}
	}
	if aFailure == nil || aFailure.RolledBack {
		t.Fatalf("expected a.go failure with RolledBack=false, got %+v", aFailure)
	}

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE target_canonical=? AND event_kind='mode_restore_failed'`, a,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 mode_restore_failed event for %s, got %d", a, n)
	}
}

// Regression for gh#122: post-commit restore-failure audit must land even
// when the caller's ctx is already cancelled. Pre-fix, AppendEvents
// opened a fresh tx under the cancelled ctx → busy_timeout scaled to ~1ms
// → audit silently dropped, leaving orphan-mode files with zero trail.
func TestAcquireLocks_AuditSurvivesCancelledCtx(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)

	ctx, cancel := context.WithCancel(context.Background())
	live := func(string, int, int64) bool { return true }
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: p},
		OwnerUUID:   tcAlice,
		SessionUUID: "s1",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, live); err != nil {
		t.Fatal(err)
	}
	cancel()
	// Drive the rollback path: rotateEventsTx etc. are committed, but a
	// future Commit-failure path mirrors restoreAllAndAudit directly.
	s.restoreAllAndAudit(ctx, []string{p}, tcAlice, time.Now())

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE target_canonical=? AND event_kind=?`,
		p, EventAcquireRollbackStart,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("acquire_rollback_started audit dropped under cancelled ctx (gh#122)")
	}
}

func TestReleaseLock_RestoresWriteMode(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }
	l := mkFileLock(t, "r.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(l.Target.Canonical); st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("precondition: acquire should strip write, got %o", st.Mode().Perm())
	}
	results, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].State != StateUnlocked {
		t.Fatalf("expected StateUnlocked, got %+v", results)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("release must restore owner-write, got %o", st.Mode().Perm())
	}
}

func TestBreakLock_RestoresWriteMode(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return false } // stale
	l := mkFileLock(t, "b.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int, int64) bool { return true }); err != nil {
		t.Fatal(err)
	}
	res, err := s.BreakLocks(ctx, []domain.Target{l.Target}, tcBob, BreakStale, "stale", "h", live)
	if err != nil || res[0].Err != nil {
		t.Fatalf("break: %v / %v", err, res[0].Err)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("break must restore owner-write, got %o", st.Mode().Perm())
	}
}

func TestReleaseLocks_NoLockVsNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	l := mkFileLock(t, "x.go", tcAlice, time.Hour)
	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice)
	if err != nil {
		t.Fatalf("ReleaseLocks (no row): %v", err)
	}
	if res[0].State != StateNoLock {
		t.Fatalf("expected StateNoLock, got %+v", res)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}
	res, err = s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob)
	if err != nil {
		t.Fatalf("ReleaseLocks (not owner): %v", err)
	}
	if res[0].State != StateNotOwner {
		t.Fatalf("expected StateNotOwner, got %+v", res)
	}
}

func TestAcquireLocks_LazyGCRestoresMode(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int, int64) bool { return false }
	live := func(string, int, int64) bool { return true }

	// Alice acquires (live probe), then her lock goes stale (probe dead).
	a := mkFileLock(t, "shared.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, live); err != nil {
		t.Fatal(err)
	}
	// Verify stripped.
	st, _ := os.Stat(a.Target.Canonical)
	if st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("acquire should strip owner-write, got %o", st.Mode().Perm())
	}

	// Bob comes along; Alice is dead → lazy GC reclaims her row, Bob acquires.
	b := a
	b.OwnerUUID = tcBob
	b.SessionUUID = tcBob
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, dead); err != nil {
		t.Fatalf("Bob acquire after stale reclaim: %v", err)
	}
	// Bob now holds; file should still be stripped (he owns it).
	st, _ = os.Stat(a.Target.Canonical)
	if st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("Bob's lock should keep write stripped, got %o", st.Mode().Perm())
	}

	// Sanity: if Bob releases, mode comes back.
	results, err := s.ReleaseLocks(ctx, []domain.Target{b.Target}, tcBob)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != StateUnlocked {
		t.Fatalf("expected StateUnlocked, got %+v", results)
	}
	st, _ = os.Stat(a.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("release should restore owner-write, got %o", st.Mode().Perm())
	}
}

func TestReleaseLocks_DistinguishesMissingFromNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	c := mkFileLock(t, "c.go", tcBob, time.Hour)
	bDir := t.TempDir()
	bPath := filepath.Join(bDir, "b.go")
	if err := os.WriteFile(bPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{c}, live); err != nil {
		t.Fatal(err)
	}

	results, err := s.ReleaseLocks(ctx, []domain.Target{
		a.Target,
		{Canonical: bPath},
		c.Target,
	}, tcAlice)
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
	stA, _ := os.Stat(a.Target.Canonical)
	if stA.Mode().Perm()&0o200 == 0 {
		t.Errorf("a.go not restored: %o", stA.Mode().Perm())
	}
	stC, _ := os.Stat(c.Target.Canonical)
	if stC.Mode().Perm()&0o222 != 0 {
		t.Errorf("c.go should remain stripped: %o", stC.Mode().Perm())
	}
}

func TestReleaseLocks_RestoreFailureIsReported(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true }

	rec := mkFileLock(t, "x.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, live); err != nil {
		t.Fatal(err)
	}

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == rec.Target.Canonical && mode.Perm()&0o200 != 0 {
			return &os.PathError{Op: "chmod", Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	results, err := s.ReleaseLocks(ctx, []domain.Target{rec.Target}, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != StateRestoreFailed {
		t.Fatalf("want StateRestoreFailed, got %+v", results)
	}
	if results[0].RestoreErr == nil {
		t.Error("RestoreErr nil")
	}
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
	if err := validateFileTarget(sym); !errors.As(err, &tve) || tve.Reason != ReasonSymlink {
		t.Fatalf("symlink: got %v, want ReasonSymlink", err)
	}

	// directory → ReasonNotRegular
	tve = nil
	if err := validateFileTarget(dir); !errors.As(err, &tve) || tve.Reason != ReasonNotRegular {
		t.Fatalf("dir: got %v, want ReasonNotRegular", err)
	}

	// hard link → ReasonMultiLinked + Nlink populated
	hard := filepath.Join(dir, "hard.go")
	if err := os.Link(realPath, hard); err != nil {
		t.Fatal(err)
	}
	tve = nil
	if err := validateFileTarget(realPath); !errors.As(err, &tve) || tve.Reason != ReasonMultiLinked {
		t.Fatalf("multi-link: got %v, want ReasonMultiLinked", err)
	}
	if tve.Nlink < 2 {
		t.Fatalf("Nlink not preserved: got %d, want >= 2", tve.Nlink)
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
	live := func(string, int, int64) bool { return true }

	dead := mkFileLock(t, "a.go", tcAlice, time.Hour) // TTL NOT expired
	dead.PID = 4242                                   // durable pid (probe will say dead)
	dead.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{dead}, live); err != nil {
		t.Fatalf("seed alice lock: %v", err)
	}

	// Probe reports pid 4242 dead → liveness-primary reclaim despite live TTL.
	deadProbe := func(host string, pid int, start int64) bool { return false }
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
	live := func(string, int, int64) bool { return true }

	held := mkFileLock(t, "a.go", tcAlice, time.Hour)
	held.PID = 4242
	held.ProcStart = 9999
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held}, live); err != nil {
		t.Fatalf("seed alice lock: %v", err)
	}

	bob := held
	bob.OwnerUUID, bob.SessionUUID, bob.PID = tcBob, tcBob, 5555
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, live)
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("bob acquire over LIVE holder must conflict, got err=%v", err)
	}
}
