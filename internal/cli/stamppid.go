package cli

import (
	"os"
	"strconv"
)

// stampPID returns the PID to stamp onto lock records. Honors LOTO_PID so a
// long-lived agent process can delegate `loto` invocations to short-lived
// wrappers (subprocesses, testscript) without their dying PIDs tripping the
// liveness probe in IsStale. Empty/invalid env → fall through to os.Getpid().
func stampPID() int {
	if s := os.Getenv("LOTO_PID"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return os.Getpid()
}
