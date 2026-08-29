//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFull forces f to stable storage. On darwin, plain fsync only reaches
// the drive's volatile write cache; F_FULLFSYNC is the real barrier (SQLite
// does the same, and so does this repo's github.com/dkoosis/atomicfile
// dependency). cmd_sync.go's publish path predates that dependency and
// hand-rolls its own temp-write/fsync/rename sequence, so it needs its own
// darwin barrier rather than atomicfile's unexported one (Codex review on
// #293). Filesystems that reject F_FULLFSYNC (some network/external
// volumes) fall back to plain fsync — weaker, but the strongest available
// there.
func syncFull(f *os.File) error {
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err == nil {
		return nil
	}
	return f.Sync()
}
