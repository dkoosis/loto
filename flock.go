package loto

import "errors"

// errFlockContention is returned by flockShared/flockExclusive when the
// non-blocking flock attempt failed because another process holds the
// lock (EWOULDBLOCK/EAGAIN). Any other error from those functions is a
// genuine syscall failure (EIO, EBADF, ENOLCK, etc.) and should propagate
// to the caller as ErrSystem rather than being conflated with contention.
var errFlockContention = errors.New("loto: flock contention")

// isFlockContention reports whether err indicates the lock is currently
// held by another process. False means err is a system-level failure.
func isFlockContention(err error) bool {
	return errors.Is(err, errFlockContention)
}
