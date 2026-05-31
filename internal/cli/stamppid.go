package cli

import (
	"os"
	"strconv"
)

// stampPID returns the PID to stamp onto lock records and whether it is a
// DURABLE liveness token. LOTO_PID (set by the SessionStart hook to the
// long-lived Claude session pid) yields (pid, true): the lock binds liveness to
// a process that outlives the one-shot CLI, so a peer can fast-reclaim it when
// the holder dies. Empty/invalid → (0, false): os.Getpid() here is the dying
// CLI pid, which would make the lock instantly reclaimable (loto-t1tq), so we
// return the PID-0 sentinel and let the caller degrade to TTL-only (loto-j1bo).
func stampPID() (pid int, durable bool) {
	if s := os.Getenv("LOTO_PID"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// degradedPidWarning returns a one-line stderr notice when we're inside a
// detectable Claude session (LOTO_AGENT_ID set) but have no durable LOTO_PID, so
// this lock is degrading to TTL-only liveness instead of fast-reclaim — i.e. the
// SessionStart hook isn't exporting LOTO_PID yet (loto-t1tq). Bare shells / cron
// (no LOTO_AGENT_ID) degrade silently: TTL-only is expected there.
func degradedPidWarning() string {
	if os.Getenv("LOTO_AGENT_ID") == "" {
		return ""
	}
	if _, durable := stampPID(); durable {
		return ""
	}
	return "LOTO_PID unset — lock liveness is TTL-only (no fast-reclaim); set LOTO_PID in the SessionStart hook. loto-t1tq\n"
}
