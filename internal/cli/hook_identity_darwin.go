//go:build darwin

package cli

import (
	"os"
	"strconv"
	"syscall"
	"time"
)

// hookFileIdentity reads the ctime, inode and device hookCacheKey needs.
func hookFileIdentity(fi os.FileInfo) (ctime time.Time, ino uint64, dev string, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return time.Time{}, 0, "", false
	}
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec), st.Ino, strconv.FormatInt(int64(st.Dev), 10), true
}
