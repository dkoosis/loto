//go:build unix

package loto

import (
	"os"
	"syscall"
)

// pidAlive reports whether pid is a running process on this host.
// Uses kill(pid, 0): succeeds if the process exists and we can signal it.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
