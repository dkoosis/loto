//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireRecoveryLock takes an exclusive flock on a sidecar file beside
// dbPath. The returned release function unflocks and closes the handle.
// Serializing recovery prevents two concurrent openers from each entering
// moveCorruptAside and racing each other's renames.
//
// Polls with LOCK_NB instead of blocking — a wedged holder (sigstopped,
// debugger-attached) used to hang the recovery path indefinitely (audit
// loto-4yt). Honors LOTO_FLOCK_TIMEOUT like acquireOpFlock.
func acquireRecoveryLock(ctx context.Context, dbPath string) (func(), error) {
	lockPath := dbPath + ".recover.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open recover lock: %w", err)
	}
	limit := flockDefaultLimit
	if s := os.Getenv("LOTO_FLOCK_TIMEOUT"); s != "" {
		if d, perr := time.ParseDuration(s); perr == nil && d > 0 {
			limit = d
		}
	}
	deadline := time.Now().Add(limit)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock recover lock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrFlockTimeout
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(jitter(flockPollInitial)):
		}
	}
}
