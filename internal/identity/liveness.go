package identity

import "os"

// SessionLiveness is the liveness verdict for the session behind a lock. THE
// one session oracle (loto-ygty): three independent approximations (lock pid
// probe, socket stat, a ps scrape) each failed in one day — worktree agents
// misread as dead (loto-r11w), PID-reuse false positives (loto-gj1z). This
// package answers the question once; the lock liveness probe consumes it.
type SessionLiveness int

const (
	// SessionLive: the recorded witnesses (socket, pid, start-time) all check
	// out — the session is provably up.
	SessionLive SessionLiveness = iota
	// SessionDead: the socket is gone, the pid is gone, or the pid's start-time
	// belongs to a different process (recycled pid).
	SessionDead
	// SessionUnknown: no session record, or one with nothing to probe — a bare
	// shell, or a session that never ran whoami. The caller falls back to its
	// own degraded signal (TTL, the lock row's pid) and MUST NOT treat this as
	// dead.
	SessionUnknown
)

func (s SessionLiveness) String() string {
	switch s {
	case SessionLive:
		return "live"
	case SessionDead:
		return "dead"
	case SessionUnknown:
		return livenessUnknownName
	default:
		return livenessUnknownName
	}
}

// livenessUnknownName is SessionUnknown's spelling, also what any value
// outside the closed set prints as.
const livenessUnknownName = "unknown"

// The oracle's machine-stable reason tokens (LivenessVerdict.Reason).
const (
	reasonNoRecord          = "no-session-record"
	reasonSocketMissing     = "socket-missing"
	reasonSocketOnly        = "socket-only"
	reasonNoWitness         = "no-witness"
	reasonPIDDead           = "pid-dead"
	reasonPID               = "pid"
	reasonProcStart         = "procstart"
	reasonProcStartMismatch = "procstart-mismatch"
)

// LivenessVerdict is the oracle's answer: the call plus the evidence that
// produced it, so CLI surfaces can print reason= without re-deriving it.
type LivenessVerdict struct {
	Liveness SessionLiveness
	Reason   string         // machine-stable token, e.g. "socket-missing", "pid-dead", "procstart-mismatch", "socket+procstart"
	Record   *SessionRecord // the record consulted; nil when none exists
	// SameProcess reports that the record was written by the process THIS
	// call is running inside. It is orthogonal to Liveness, not a flavour of
	// it: Claude Code's /clear rotates the session id and leaves the process
	// running, so the previous id's record stays honestly live while naming
	// nobody else (loto-2jgn). A caller counting PEERS subtracts these; a
	// caller asking "may I reclaim what this session holds" must not, since
	// the process behind it is up. False whenever either side's process
	// identity is unknown — absence of evidence is not evidence of sameness.
	SameProcess bool
}

// procStartFn is ProcStart, indirected as a test seam. Never nil in
// production; tests restore it via t.Cleanup.
var procStartFn = ProcStart //nolint:gochecknoglobals // test seam

// ProbeSession answers "is the session sid still up?" from this host's session
// records. No record → SessionUnknown: the caller keeps its own degraded
// signal and must not reclaim on that answer alone.
func ProbeSession(sid string) LivenessVerdict {
	rec, ok := readSession(sid)
	if !ok {
		return LivenessVerdict{Liveness: SessionUnknown, Reason: reasonNoRecord}
	}
	v := rec.Verdict()
	v.SameProcess = rec.sameProcess(callerProc())
	return v
}

// Verdict is the oracle's decision procedure over one session record:
//
//	socket recorded, missing   → dead    (the session process removes it by dying)
//	pid unknown, socket exists → live    (the socket is the witness)
//	pid unknown, no socket     → unknown (nothing to probe)
//	pid not running            → dead
//	start-time mismatch        → dead    (OS recycled the pid to another process)
//	start-time match           → live    (proven identical process)
//	otherwise                  → live    (pid alive; no start-time to contradict it)
//
// The start-time witness runs when the record carries one: pid's OS
// start-time is re-read and compared for EQUALITY (no tolerance — same-host,
// same-reader values need none) against the one recorded at RecordSession
// time. An unreadable start-time (ProcStart == 0, or the verdict-time read
// fails) is NOT evidence and never escalates to dead — indeterminate reads
// degrading to "dead" is exactly the r11w failure shape, inverted.
func (r SessionRecord) Verdict() LivenessVerdict {
	rec := &r
	if r.Socket != "" {
		if _, err := os.Stat(r.Socket); err != nil {
			return LivenessVerdict{Liveness: SessionDead, Reason: reasonSocketMissing, Record: rec}
		}
	}
	if r.PID <= 0 {
		if r.Socket != "" {
			return LivenessVerdict{Liveness: SessionLive, Reason: reasonSocketOnly, Record: rec}
		}
		return LivenessVerdict{Liveness: SessionUnknown, Reason: reasonNoWitness, Record: rec}
	}
	if !PIDAlive(r.PID) {
		return LivenessVerdict{Liveness: SessionDead, Reason: reasonPIDDead, Record: rec}
	}
	if r.ProcStart != 0 {
		if cur, ok := procStartFn(r.PID); ok {
			if cur != r.ProcStart {
				return LivenessVerdict{Liveness: SessionDead, Reason: reasonProcStartMismatch, Record: rec}
			}
			return LivenessVerdict{Liveness: SessionLive, Reason: witnessReason(r.Socket, reasonProcStart), Record: rec}
		}
	}
	return LivenessVerdict{Liveness: SessionLive, Reason: witnessReason(r.Socket, reasonPID), Record: rec}
}

// witnessReason prefixes a pid-side reason with "socket+" when the socket
// also witnessed, so the token says which evidence carried the verdict.
func witnessReason(socket, pidReason string) string {
	if socket != "" {
		return "socket+" + pidReason
	}
	return pidReason
}
