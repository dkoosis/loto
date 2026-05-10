//go:build unix

package loto

import (
	"errors"
	"os"
	"syscall"
)

// fdAsInt converts an *os.File's file descriptor to int for syscall.Flock.
// File descriptors fit comfortably in int on every supported platform; the
// uintptr→int conversion cannot overflow in practice.
func fdAsInt(f *os.File) int {
	return int(f.Fd())
}

// flockShared takes a non-blocking shared (read) lock on f.
// Returns errFlockContention if the lock is held by another process;
// any other non-nil error is a system-level failure.
func flockShared(f *os.File) error {
	return wrapFlockErr(syscall.Flock(fdAsInt(f), syscall.LOCK_SH|syscall.LOCK_NB))
}

// flockExclusive takes a non-blocking exclusive (write) lock on f.
// Returns errFlockContention if the lock is held by another process;
// any other non-nil error is a system-level failure.
func flockExclusive(f *os.File) error {
	return wrapFlockErr(syscall.Flock(fdAsInt(f), syscall.LOCK_EX|syscall.LOCK_NB))
}

// wrapFlockErr maps EWOULDBLOCK/EAGAIN to errFlockContention and passes
// every other error through untouched. Without this discrimination,
// callers misreport real syscall failures (EIO, EBADF, ENOLCK) as "held
// by X", triggering retry loops on hardware/filesystem outages.
func wrapFlockErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errFlockContention
	}
	return err
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
