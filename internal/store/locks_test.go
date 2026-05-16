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
	live := func(string, int) bool { return true }
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
	live := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, live); err != nil {
		t.Fatal(err)
	}

	if err := s.BreakLock(ctx, l.Target, tcBob, false, tcTest, live); err == nil {
		t.Fatal("live break without force must fail")
	}
	if err := s.BreakLock(ctx, l.Target, tcBob, true, "deadline", live); err != nil {
		t.Fatalf("force break: %v", err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("lock should be gone, got %+v", got)
	}
	events, _ := s.EventsForTarget(ctx, l.Target)
	if len(events) != 1 || events[0].Kind != EventLockBroken {
		t.Fatalf("expected single lock_broken event, got %+v", events)
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

// mkFileLock creates a real file under t.TempDir() and returns a LockRecord
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
		OwnerUUID:   agent,
		SessionUUID: agent,
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
	live := func(string, int) bool { return true }

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lb := mkFileLock(t, "b.go", tcAlice, time.Hour)
	lc := mkFileLock(t, "c.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb, lc}, live); err != nil {
		t.Fatal(err)
	}

	targets := []domain.Target{la.Target, lb.Target, lc.Target}
	results, err := s.BreakLocks(ctx, targets, tcBob, true, "batch break", live)
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
		if len(evs) != 1 || evs[0].Kind != EventLockBroken {
			t.Errorf("targets[%d] expected one lock_broken event, got %+v", i, evs)
		}
	}
}

func TestBreakLocks_MixedNoLockAndOwned(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }

	la := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la}, live); err != nil {
		t.Fatal(err)
	}
	missing := domain.Target{Canonical: filepath.Join(t.TempDir(), "missing.go")}
	if err := os.WriteFile(missing.Canonical, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := s.BreakLocks(ctx, []domain.Target{la.Target, missing}, tcBob, true, "mixed", live)
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
	live := func(string, int) bool { return true }

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
	if res[1].State != StateNotOwner || res[1].Holder != tcBob {
		t.Errorf("res[1]: want StateNotOwner holder=bob, got state=%v holder=%v", res[1].State, res[1].Holder)
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
	live := func(string, int) bool { return true }
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

func TestAcquireSameAgentRefreshes(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
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
	live := func(string, int) bool { return true }
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   owner,
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
	live := func(string, int) bool { return true }
	now := time.Now()
	mk := func(p, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   owner,
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

	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		if path == b {
			return &os.PathError{Op: tcChmod, Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}

	live := func(string, int) bool { return true }
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

	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		switch {
		case path == b:
			return &os.PathError{Op: tcChmod, Path: path, Err: syscall.EPERM}
		case path == a && mode.Perm()&0o200 != 0:
			return &os.PathError{Op: tcChmod, Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}

	live := func(string, int) bool { return true }
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

func TestReleaseLock_RestoresWriteMode(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }
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
	live := func(string, int) bool { return false } // stale
	l := mkFileLock(t, "b.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err := s.BreakLock(ctx, l.Target, tcBob, false, "stale", live); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("break must restore owner-write, got %o", st.Mode().Perm())
	}
}

func TestReleaseLocks_NoLockVsNotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int) bool { return true }

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
	dead := func(string, int) bool { return false }
	live := func(string, int) bool { return true }

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
	live := func(string, int) bool { return true }

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
	if results[2].Holder != tcBob {
		t.Errorf("results[2].Holder = %q, want %q", results[2].Holder, tcBob)
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
	live := func(string, int) bool { return true }

	rec := mkFileLock(t, "x.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, live); err != nil {
		t.Fatal(err)
	}

	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		if path == rec.Target.Canonical && mode.Perm()&0o200 != 0 {
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
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
