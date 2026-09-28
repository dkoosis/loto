//go:build !darwin && !(linux && (amd64 || arm64))

package cli

import (
	"os"
	"time"
)

// hookFileIdentity has no portable ctime or inode here, so hookCacheKey
// returns "" and every locked path is hashed on every call — correct, and
// only slower.
func hookFileIdentity(os.FileInfo) (ctime time.Time, ino uint64, dev string, ok bool) {
	return time.Time{}, 0, "", false
}
