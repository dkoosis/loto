//go:build linux

package cli

import (
	"os"
	"strconv"
	"strings"
)

// procStart returns pid's start-time in clock ticks since boot, read from
// /proc/<pid>/stat field 22 (starttime). The second return is false when the
// value can't be read (pid gone, /proc unavailable) — caller treats false as
// UNKNOWN and falls back to a plain pid-alive check. The encoding is opaque:
// it is only ever compared for equality against a value read on the same host
// (see domain.LockRecord.ProcStart).
func procStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(b)
	// Field 2 (comm) is wrapped in parens and may itself contain spaces or
	// ')'. The standard trick: split on the LAST ')' — everything after it is
	// the space-separated remainder starting at field 3 (state).
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 > len(s) {
		return 0, false
	}
	rest := strings.Fields(s[rparen+2:])
	// rest[0] == field 3 (state); field 22 (starttime) is rest[19].
	const starttimeIdx = 19
	if len(rest) <= starttimeIdx {
		return 0, false
	}
	v, err := strconv.ParseInt(rest[starttimeIdx], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
