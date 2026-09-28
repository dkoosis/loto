//go:build linux && (amd64 || arm64)

package cli

import (
	"os"
	"strconv"
	"syscall"
	"time"
)

// hookFileIdentity reads the ctime, inode and device hookCacheKey needs.
// 64-bit linux only: there every Stat_t field used here is already 64 bits
// wide, so no conversion is needed. Other linux arches take the no-cache
// fallback in hook_identity_other.go.
func hookFileIdentity(fi os.FileInfo) (ctime time.Time, ino uint64, dev string, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return time.Time{}, 0, "", false
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec), st.Ino, strconv.FormatUint(st.Dev, 10), true
}
