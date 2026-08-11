//go:build unix

package identity

import (
	"errors"
	"syscall"
)

var killFn = syscall.Kill //nolint:gochecknoglobals // test seam for the signal-0 probe

// PIDAlive reports whether pid is currently running. Moved from internal/cli
// (loto-ygty): the liveness oracle lives here, and the signal-0 probe is one of
// its inputs, so the one implementation moved to the one consumer both layers
// share.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := killFn(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the pid exists but is owned by another UID — still live.
	return errors.Is(err, syscall.EPERM)
}
