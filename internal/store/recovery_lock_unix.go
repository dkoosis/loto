//go:build unix

package store

import (
	"fmt"
	"os"
	"syscall"
)

// acquireRecoveryLock takes an exclusive flock on a sidecar file beside
// dbPath. The returned release function unflocks and closes the handle.
// Serializing recovery prevents two concurrent openers from each entering
// MoveCorruptAside and racing each other's renames.
func acquireRecoveryLock(dbPath string) (func(), error) {
	lockPath := dbPath + ".recover.lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open recover lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock recover lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
