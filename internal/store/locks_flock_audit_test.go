package store

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestOpFlockReleasedBeforeDetachedAudit covers loto-3qev: doctor/break/release
// must release the op-flock BEFORE the detached audit write tx, not span it. The
// bug held the flock across restoreReclaimedAndAudit / restoreAndAuditBreaks /
// restoreAndAuditReleases, so a chmod-restore failure that triggers a detached
// audit (which opens its own write tx and can poll busy_timeout under contention)
// kept every peer stalled behind the op-flock for that window. The acquire path
// already splits this (restoreThenReleaseFlock); this asserts the other three do.
//
// Probe: force the post-commit chmod restore to fail so a detached audit fires.
// Hook the detached-audit entry (auditDetachedHook) — at that instant the
// op-flock MUST already be free, so a non-blocking flock try from a SECOND
// handle on the same op-flock path succeeds. If the flock were still held across
// the audit (the bug), the try would get EWOULDBLOCK.
func TestOpFlockReleasedBeforeDetachedAudit(t *testing.T) {
	tests := []struct {
		name string
		// run performs the operation under test on a target that AcquireLocks has
		// already locked EXCLUSIVE (write bit stripped). The chmod-restore is
		// rigged to fail, so each op schedules a detached mode_restore_failed audit.
		run func(t *testing.T, s *Store, ctx context.Context, target domain.Target, live domain.PidLiveProbe)
	}{
		{
			name: "DoctorRepair",
			run: func(t *testing.T, s *Store, ctx context.Context, target domain.Target, live domain.PidLiveProbe) {
				t.Helper()
				if err := s.DoctorRepair(ctx, "h", tcBob, live); err != nil {
					t.Fatalf("DoctorRepair: %v", err)
				}
			},
		},
		{
			name: "BreakLocks",
			run: func(t *testing.T, s *Store, ctx context.Context, target domain.Target, live domain.PidLiveProbe) {
				t.Helper()
				if _, err := s.BreakLocks(ctx, []domain.Target{target}, tcBob, BreakForce, "r", "h", live); err != nil {
					t.Fatalf("BreakLocks: %v", err)
				}
			},
		},
		{
			name: "ReleaseLocks",
			run: func(t *testing.T, s *Store, ctx context.Context, target domain.Target, live domain.PidLiveProbe) {
				t.Helper()
				if _, err := s.ReleaseLocks(ctx, []domain.Target{target}, tcAlice); err != nil {
					t.Fatalf("ReleaseLocks: %v", err)
				}
			},
		},
		{
			name: "ReleaseBySession",
			run: func(t *testing.T, s *Store, ctx context.Context, target domain.Target, live domain.PidLiveProbe) {
				t.Helper()
				if _, err := s.ReleaseBySession(ctx, tcAlice, tcAlice); err != nil {
					t.Fatalf("ReleaseBySession: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustOpen(t)
			ctx := context.Background()
			// pid-dead → the lock reads stale, which DoctorRepair needs to reclaim.
			live := func(string, int, int64) bool { return false }

			// Alice holds an EXCLUSIVE lock (write bit stripped on acquire). For the
			// DoctorRepair case the lock must read stale, so expire it; the other
			// ops act on it directly regardless of expiry.
			expiry := time.Hour
			if tt.name == "DoctorRepair" {
				expiry = -time.Hour
			}
			alice := mkFileLock(t, "a.go", tcAlice, expiry)
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, live); err != nil {
				t.Fatalf("seed acquire: %v", err)
			}
			target := alice.Target

			// Make the post-commit restore (add owner-write) fail so a detached
			// mode_restore_failed audit fires. The strip during acquire already ran;
			// flip fchmodFn now so only the restore returns EPERM.
			origChmod := fchmodFn
			defer func() { fchmodFn = origChmod }()
			fchmodFn = func(f *os.File, mode os.FileMode) error {
				if f.Name() == target.Canonical {
					return &os.PathError{Op: "chmod", Path: f.Name(), Err: syscall.EPERM}
				}
				return origChmod(f, mode)
			}

			// At detached-audit time, probe the op-flock from a second handle. With
			// the fix the op-flock is already released, so a non-blocking LOCK_EX
			// succeeds; the bug would yield EWOULDBLOCK.
			flockPath := s.opFlockPath()
			var (
				mu          sync.Mutex
				hookRan     bool
				flockFreeAt bool
			)
			origHook := auditDetachedHook
			defer func() { auditDetachedHook = origHook }()
			auditDetachedHook = func() {
				free := tryFlockFree(t, flockPath)
				mu.Lock()
				hookRan = true
				flockFreeAt = free
				mu.Unlock()
			}

			tt.run(t, s, ctx, target, live)

			mu.Lock()
			defer mu.Unlock()
			if !hookRan {
				t.Fatalf("detached audit never ran — chmod-restore-failure path not exercised")
			}
			if !flockFreeAt {
				t.Errorf("op-flock still held during detached audit write — release spans the audit (loto-3qev)")
			}
		})
	}
}

// tryFlockFree opens a SECOND handle on the op-flock path and attempts a
// non-blocking exclusive flock. Returns true when the lock is free (acquired),
// false on EWOULDBLOCK (still held by the operation under test). It releases any
// lock it took so it never leaks into the operation's own release.
func tryFlockFree(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open op-flock probe: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return true
	}
	return false
}
