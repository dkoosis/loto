//go:build unix

package loto

import (
	"os"
	"syscall"
)

// fdAsInt converts an *os.File's file descriptor to int for syscall.Flock.
// File descriptors fit comfortably in int on every supported platform; the
// uintptr→int conversion cannot overflow in practice.
func fdAsInt(f *os.File) int {
	return int(f.Fd()) //nolint:gosec // G115: fd always fits in int
}

// flockShared takes a non-blocking shared (read) lock on f.
func flockShared(f *os.File) error {
	return syscall.Flock(fdAsInt(f), syscall.LOCK_SH|syscall.LOCK_NB)
}

// flockExclusive takes a non-blocking exclusive (write) lock on f.
func flockExclusive(f *os.File) error {
	return syscall.Flock(fdAsInt(f), syscall.LOCK_EX|syscall.LOCK_NB)
}

// flockExclusiveBlocking takes a blocking exclusive lock on f, waiting until
// the current holder releases. Used only by ForceBreak.
func flockExclusiveBlocking(f *os.File) error {
	return syscall.Flock(fdAsInt(f), syscall.LOCK_EX)
}

// flockRelease releases any lock held on f.
func flockRelease(f *os.File) error {
	return syscall.Flock(fdAsInt(f), syscall.LOCK_UN)
}
