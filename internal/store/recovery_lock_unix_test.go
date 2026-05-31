//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// holdRecoveryLock opens an exclusive flock on the .recover.lock sidecar file
// for dbPath and returns a release function. Uses raw syscall so the test does
// not depend on acquireRecoveryLock itself for the holder side.
func holdRecoveryLock(t *testing.T, dbPath string) func() {
	t.Helper()
	lockPath := dbPath + ".recover.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("holdRecoveryLock: open: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("holdRecoveryLock: flock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// TestRecoveryLock_TimeoutUnderContention verifies that acquireRecoveryLock
// returns ErrFlockTimeout when a second caller cannot take the lock within
// LOTO_FLOCK_TIMEOUT. This covers recovery_lock_unix.go:47-49.
func TestRecoveryLock_TimeoutUnderContention(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	// Holder takes the raw lock — simulates another process in recovery.
	release := holdRecoveryLock(t, dbPath)
	defer release()

	t.Setenv("LOTO_FLOCK_TIMEOUT", "150ms")

	start := time.Now()
	_, err := acquireRecoveryLock(context.Background(), dbPath)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrFlockTimeout) {
		t.Fatalf("want ErrFlockTimeout, got %v", err)
	}
	// Must not wait much longer than the configured timeout.
	if elapsed > 800*time.Millisecond {
		t.Errorf("timeout took too long: %v (want ≤800ms)", elapsed)
	}
}

// TestRecoveryLock_CtxCancelAbortsEarly verifies that a cancelled context
// causes acquireRecoveryLock to return ctx.Err() before the flock timeout
// fires. Covers recovery_lock_unix.go:51-54.
func TestRecoveryLock_CtxCancelAbortsEarly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	// Holder holds the lock so the second acquire enters the poll loop.
	release := holdRecoveryLock(t, dbPath)
	defer release()

	// Long flock timeout — ctx cancel must preempt it.
	t.Setenv("LOTO_FLOCK_TIMEOUT", "10s")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := acquireRecoveryLock(ctx, dbPath)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("ctx cancel didn't preempt promptly: %v", elapsed)
	}
}

// TestRecoveryLock_DeadlineExceededReturnsCtxErr verifies that a context
// deadline (not just cancellation) also terminates the poll loop via ctx.Err()
// when the deadline fires before LOTO_FLOCK_TIMEOUT. Covers the same
// ctx.Done() branch with a different error value (DeadlineExceeded).
func TestRecoveryLock_DeadlineExceededReturnsCtxErr(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	release := holdRecoveryLock(t, dbPath)
	defer release()

	// Flock timeout is very long; context deadline fires first.
	t.Setenv("LOTO_FLOCK_TIMEOUT", "10s")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := acquireRecoveryLock(ctx, dbPath)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("deadline didn't preempt promptly: %v", elapsed)
	}
}

// TestRecoveryLock_AcquiresWhenFree verifies the happy path: no contention,
// lock is taken, release is returned without error.
func TestRecoveryLock_AcquiresWhenFree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	release, err := acquireRecoveryLock(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if release == nil {
		t.Fatal("want non-nil release func, got nil")
	}
	release()
}

// TestRecoveryLock_SerializesTwoConcurrentCallers verifies that two goroutines
// calling acquireRecoveryLock do NOT hold the lock simultaneously — they are
// serialized. The goroutine that acquires first holds it for a fixed window;
// the second must wait and then succeed after the first releases.
func TestRecoveryLock_SerializesTwoConcurrentCallers(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	// Long enough timeout for both goroutines to eventually succeed.
	t.Setenv("LOTO_FLOCK_TIMEOUT", "5s")

	type event struct {
		id    int
		start time.Time
		end   time.Time
	}

	results := make(chan event, 2)
	errs := make(chan error, 2)

	for i := range 2 {
		go func(id int) {
			release, err := acquireRecoveryLock(context.Background(), dbPath)
			if err != nil {
				errs <- err
				return
			}
			s := time.Now()
			time.Sleep(40 * time.Millisecond) // hold briefly
			e := time.Now()
			release()
			results <- event{id, s, e}
		}(i)
	}

	var events [2]event
	for i := range 2 {
		select {
		case ev := <-results:
			events[i] = ev
		case err := <-errs:
			t.Fatalf("goroutine failed: %v", err)
		case <-time.After(6 * time.Second):
			t.Fatal("timed out waiting for both goroutines")
		}
	}

	// Intervals must not overlap — one ends before the other starts.
	a, b := events[0], events[1]
	if a.start.After(b.start) {
		a, b = b, a // sort by start time
	}
	if a.end.After(b.start) {
		t.Errorf("lock held concurrently: [%v-%v] overlaps [%v-%v]",
			a.start, a.end, b.start, b.end)
	}
}

// TestRecoveryLock_ReleaseAllowsReacquire verifies that releasing the lock
// allows a subsequent acquireRecoveryLock to succeed (no fd leak / lock stuck).
func TestRecoveryLock_ReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	release, err := acquireRecoveryLock(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	// Should succeed immediately — no leaked lock.
	t.Setenv("LOTO_FLOCK_TIMEOUT", "500ms")
	release2, err := acquireRecoveryLock(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	release2()
}
