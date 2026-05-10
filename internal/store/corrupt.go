package store

import (
	"fmt"
	"os"
	"time"
)

// MoveCorruptAside renames a corrupt DB to <path>.corrupt.<unix-nanos> so a fresh
// DB can be created. Full implementation lives with Task 10 (doctor); this is the
// minimal version called by Open() retry path.
func MoveCorruptAside(path string, now time.Time) (string, error) {
	dst := fmt.Sprintf("%s.corrupt.%d", path, now.UnixNano())
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	// Best-effort sidecar removal; ignore errors.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Rename(path+suffix, dst+suffix)
	}
	return dst, nil
}
