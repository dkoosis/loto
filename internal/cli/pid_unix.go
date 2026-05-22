//go:build unix

package cli

import (
	"errors"
	"syscall"
)

var killFn = syscall.Kill

func pidLive(pid int) bool {
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
