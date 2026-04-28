//go:build unix

package loto

import (
	"os"
	"syscall"
)

// flockShared takes a non-blocking shared (read) lock on f.
func flockShared(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

// flockExclusive takes a non-blocking exclusive (write) lock on f.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
