//go:build !unix

package loto

import (
	"errors"
	"os"
)

// errUnsupported is returned on platforms without flock(2). A Windows
// implementation using LockFileEx is straightforward; left for a follow-up.
var errUnsupported = errors.New("loto: file locking not supported on this platform")

func flockShared(f *os.File) error    { return errUnsupported }
func flockExclusive(f *os.File) error { return errUnsupported }
