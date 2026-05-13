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

	if err := s.ReleaseLock(ctx, l.Target, tcBob); err == nil {
		t.Fatal("non-owner release must fail")
	}
	if err := s.ReleaseLock(ctx, l.Target, tcAlice); err != nil {
		t.Fatalf("owner release: %v", err)
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
