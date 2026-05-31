package cli

import (
	"os"
	"strconv"
)

// pidSource classifies how LOTO_PID resolved, so callers branch on the reason
// without re-reading the env. Only pidDurable yields a usable liveness pid.
type pidSource int

const (
	pidDurable pidSource = iota // valid positive LOTO_PID — the long-lived session pid
	pidUnset                    // LOTO_PID not set (bare shell / cron / hook not exporting it)
	pidInvalid                  // LOTO_PID set but not a positive int
)

// stampPID returns the PID to stamp onto lock records and the source that
// produced it. A valid LOTO_PID (set by the SessionStart hook to the long-lived
// Claude session pid) yields (pid, pidDurable): the lock binds liveness to a
// process that outlives the one-shot CLI, so a peer can fast-reclaim it when the
// holder dies. Anything else yields (0, pidUnset|pidInvalid) — the PID-0
// sentinel — because stamping os.Getpid() (the dying CLI pid) would make the
// lock instantly reclaimable (loto-t1tq); the caller degrades to TTL-only
// (loto-j1bo).
func stampPID() (pid int, src pidSource) {
	s := os.Getenv("LOTO_PID")
	if s == "" {
		return 0, pidUnset
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n, pidDurable
	}
	return 0, pidInvalid
}

// degradedPidWarning returns a one-line stderr notice when we're inside a
// detectable Claude session (LOTO_AGENT_ID set) but have no durable LOTO_PID, so
// this lock is degrading to TTL-only liveness instead of fast-reclaim — i.e. the
// SessionStart hook isn't exporting a valid LOTO_PID yet (loto-t1tq). Bare
// shells / cron (no LOTO_AGENT_ID) degrade silently: TTL-only is expected there.
func degradedPidWarning() string {
	if os.Getenv("LOTO_AGENT_ID") == "" {
		return ""
	}
	switch _, src := stampPID(); src {
	case pidInvalid:
		return "LOTO_PID invalid (want positive int) — lock liveness is TTL-only (no fast-reclaim); fix it in the SessionStart hook. loto-t1tq\n"
	case pidUnset:
		return "LOTO_PID unset — lock liveness is TTL-only (no fast-reclaim); set LOTO_PID in the SessionStart hook. loto-t1tq\n"
	case pidDurable:
		return ""
	default:
		return ""
	}
}
