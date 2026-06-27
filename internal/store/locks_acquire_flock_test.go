package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestAcquireLocks_HoldsFlockAcrossRestore covers loto-v8ch: the op-flock must
// be held across the post-commit FS restore, not released before it. The bug
// was a `flock.release()` between commit and restoreReclaimedSkippingRestripped,
// opening a torn-view window: a peer could acquire the flock, read the
// consistent DB (stale row gone), and either hand the user a target still
// chmod read-only, or — the worse interleaving — acquire exclusive and re-strip,
// after which the lagging restore re-adds owner-write under the peer's lease and
// silently defeats its exclusivity.
//
// Scenario: Bob holds a stale EXCLUSIVE lock (write bit stripped); Alice acquires
// SHARED over it (reclaims Bob's row, schedules a post-commit restore). We hook
// the commit so that, the instant Alice's tx commits, a peer goroutine attempts
// its own EXCLUSIVE acquire on the same target. With the flock correctly held
// across restore, the peer blocks until Alice finishes; it then strips the bit
// and the target ends read-only. If the flock were released before restore, the
// peer could strip and Alice's restore would race in afterward, leaving the
// target writable under the peer's exclusive lease.
func TestAcquireLocks_HoldsFlockAcrossRestore(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	live := func(string, int, int64) bool { return true } // pid-live not consulted; staleness drives reclaim

	// Bob holds a stale EXCLUSIVE lock — already expired, so Alice's acquire
	// reclaims it. mkFileLock strips owner-write on exclusive acquire.
	bob := mkFileLock(t, "shared.go", tcBob, -time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, live); err != nil {
		t.Fatalf("seed bob acquire: %v", err)
	}
	target := bob.Target.Canonical
	if !readOnly(t, target) {
		t.Fatalf("precondition: bob's exclusive should have stripped owner-write")
	}

	// Alice acquires SHARED on the same target. Reclaiming bob's stale EXCLUSIVE
	// row schedules a post-commit restoreWrite. Build a same-target record under
	// alice with shared mode.
	alice := bob
	alice.OwnerUUID = tcAlice
	alice.SessionUUID = tcAlice
	alice.Mode = domain.ModeShared
	// Expire alice's row so the peer's later EXCLUSIVE acquire reclaims it as
	// stale (a live shared peer would otherwise block exclusive — correct
	// semantics, but not the window under test). Reclaiming a SHARED row does
	// not restore a write bit (shouldRestoreOwnerWrite gates on exclusive), so
	// the peer's strip is the only thing that should touch the bit after this.
	alice.ExpiresAt = time.Now().Add(-time.Hour)

	// Peer acquires EXCLUSIVE on the same target — must end up holding it and
	// keeping the bit OFF. Run from a goroutine kicked off the instant alice's
	// commit lands (between commit and restore).
	const peer = "carol"
	peerRec := bob
	peerRec.OwnerUUID = peer
	peerRec.SessionUUID = peer
	peerRec.ExpiresAt = time.Now().Add(time.Hour)

	// Slow the restore (owner-write add) to bias the BUG's race the way that
	// surfaces it: in buggy code (flock released before restore) the already-
	// parked peer wins the flock and strips while this delay is in flight, then
	// the lagging restore re-adds owner-write → target ends WRITABLE. This is a
	// deliberate window-widener for bug detection, not a readiness primitive
	// (the readiness wait below is now a deterministic handoff, loto-b20a). With
	// the flock correctly held across restore, the peer can't proceed until
	// restore finishes, so the delay is harmless. Fires only for the add-write
	// chmod on our target (perm has 0o200 set).
	origChmod := fchmodFn
	defer func() { fchmodFn = origChmod }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == target && mode&0o200 != 0 {
			time.Sleep(100 * time.Millisecond)
		}
		return origChmod(f, mode)
	}

	// peerBlocked closes the instant the peer's acquireOpFlock first observes the
	// flock held (EWOULDBLOCK) — i.e. the peer is provably parked on the flock we
	// still hold. Replaces a 50ms readiness sleep that, if too short on a loaded
	// runner, let alice release-and-restore before the peer ever contended,
	// silently false-passing the very torn-view race under test (loto-b20a/v8ch).
	peerBlocked := make(chan struct{})
	var blockOnce sync.Once
	origContended := flockContendedFn
	defer func() { flockContendedFn = origContended }()
	flockContendedFn = func() { blockOnce.Do(func() { close(peerBlocked) }) }

	var wg sync.WaitGroup
	var peerErr error
	hookFired := false

	orig := commitTxFn
	defer func() { commitTxFn = orig }()
	commitTxFn = func(tx *sql.Tx) error {
		err := orig(tx)
		// Only the success-path commit on alice's parent tx; fire once.
		if err == nil && !hookFired {
			hookFired = true
			// Restore commitTxFn to the real impl so the peer's own commit isn't
			// re-hooked (re-entrancy + recursion guard).
			commitTxFn = orig
			wg.Go(func() {
				_, peerErr = s.AcquireLocks(ctx, []domain.LockRecord{peerRec}, live)
			})
			// Wait until the peer is provably parked on the flock we still hold —
			// deterministic handoff, no timing guess. alice cannot release/restore
			// until the peer has contended, so the peer is positioned to win the
			// flock the moment (correct: never; buggy: at the early release). The
			// timeout fails fast instead of hanging the suite to the go-test
			// deadline if a regression stops the peer from ever blocking.
			select {
			case <-peerBlocked:
			case <-time.After(5 * time.Second):
				t.Fatal("peer never blocked on the op-flock — deterministic handoff broke")
			}
		}
		return err
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, live); err != nil {
		t.Fatalf("alice shared acquire: %v", err)
	}
	wg.Wait()

	if peerErr != nil {
		t.Fatalf("peer exclusive acquire: %v", peerErr)
	}
	// The peer holds EXCLUSIVE; the target MUST be read-only. A writable target
	// here means alice's restore re-added owner-write after the peer stripped it
	// — the exact exclusivity-defeating bug loto-v8ch fixes.
	if !readOnly(t, target) {
		t.Errorf("target writable under peer's exclusive lease — restore raced the flock release (loto-v8ch)")
	}
}

func readOnly(t *testing.T, path string) bool {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Mode().Perm()&0o200 == 0
}
