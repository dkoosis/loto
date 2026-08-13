package identity

import (
	"context"
	"os"
)

// SessionLiveness is the fleet-wide liveness verdict for the session behind an
// agent. THE one liveness oracle (loto-ygty): three independent approximations
// (lock pid probe, who's socket stat, the bdx reaper's ps scrape) each failed
// in one day — worktree agents misread as dead (loto-r11w), PID-reuse false
// positives (loto-gj1z), a reaper armed against a live agent (ccp#130). This
// package now answers the question once; locks, who, and the reaper consume it.
type SessionLiveness int

const (
	// SessionLive: the messaging socket exists AND nothing contradicts the
	// recorded identity — the session is provably up.
	SessionLive SessionLiveness = iota
	// SessionDead: the socket is gone, its pid is gone, or the pid's argv
	// belongs to a different process (recycled pid under an orphaned socket).
	SessionDead
	// SessionUnknown: no peer record — a bare shell, cron, or a session that
	// predates peer recording. Nothing to probe; the caller falls back to its
	// own degraded signal (TTL, pid) and MUST NOT treat this as dead.
	SessionUnknown
)

func (s SessionLiveness) String() string {
	switch s {
	case SessionLive:
		return "live"
	case SessionDead:
		return "dead"
	case SessionUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// LivenessVerdict is the oracle's answer: the call plus the evidence that
// produced it, so CLI surfaces can print reason= without re-deriving it.
type LivenessVerdict struct {
	Liveness SessionLiveness
	Reason   string // machine-stable token, e.g. "socket+ps-match", "socket-missing", "socket+procstart", "procstart-mismatch"
	Peer     *Peer  // the record consulted; nil when none exists
}

// procStartFn is ProcStart, indirected as a test seam. Never nil in
// production; tests restore it via t.Cleanup, mirroring procArgv.
var procStartFn = ProcStart //nolint:gochecknoglobals // test seam, matches procArgv's pattern

// SessionVerdict is the oracle's decision procedure over one peer record:
//
//	socket missing            → dead   (the session process removes it by dying)
//	socket pid not running    → dead   (orphaned socket file)
//	start-time mismatch       → dead   (OS recycled the pid to another process)
//	start-time match          → live   (proven identical process; skips the ps exec below)
//	argv identity mismatch    → dead   (OS recycled the pid to another process)
//	otherwise                 → live
//
// The start-time witness runs first, when the record carries one: pid's OS
// start-time is re-read and compared for EQUALITY (no tolerance — same-host,
// same-reader values need none) against the one recorded at RecordPeer time.
// A mismatch means the OS recycled the pid, so this closes the case the argv
// check cannot: a top-level session records neither --agent-id nor
// --parent-session-id (see PeerFromEnv), so argv mismatch alone never fires
// for it. A match proves process identity and short-circuits the ps exec
// below — argv is a strictly weaker proxy for the same fact. An unreadable
// start-time (ProcStart == 0, or the verdict-time read fails) is NOT
// evidence and falls through to the argv check, never straight to dead.
//
// The argv check compares the flags recorded at RecordPeer time (--agent-id,
// --parent-session-id) against the pid's current command line. A flag recorded
// non-empty that is now absent or different means the occupant is not the
// process that recorded the peer. An unreadable proc table never escalates to
// dead — the socket already witnessed the session, and indeterminate reads
// degrading to "dead" is exactly the r11w failure shape, inverted.
func (p Peer) SessionVerdict(ctx context.Context) LivenessVerdict {
	if p.Socket == "" {
		return LivenessVerdict{Liveness: SessionUnknown, Reason: "no-socket-recorded", Peer: &p}
	}
	if _, err := os.Stat(p.Socket); err != nil {
		return LivenessVerdict{Liveness: SessionDead, Reason: "socket-missing", Peer: &p}
	}
	if p.PID <= 0 {
		// Socket exists but no pid to cross-check; the socket is the witness.
		return LivenessVerdict{Liveness: SessionLive, Reason: "socket-only", Peer: &p}
	}
	if !PIDAlive(p.PID) {
		return LivenessVerdict{Liveness: SessionDead, Reason: "pid-dead", Peer: &p}
	}
	if v, ok := startTimeVerdict(p); ok {
		return v
	}
	argv := procArgv(ctx, p.PID)
	if argv == "" {
		return LivenessVerdict{Liveness: SessionLive, Reason: "socket+pid", Peer: &p}
	}
	if mismatch(argv, "agent-id", p.AgentID) || mismatch(argv, "parent-session-id", p.ParentSessionID) {
		return LivenessVerdict{Liveness: SessionDead, Reason: "argv-mismatch", Peer: &p}
	}
	return LivenessVerdict{Liveness: SessionLive, Reason: "socket+ps-match", Peer: &p}
}

// startTimeVerdict is SessionVerdict's start-time witness, split out to keep
// SessionVerdict under the cyclop/gocognit branch limit. ok is true only when
// the start-time proved the verdict one way or the other (mismatch → dead,
// match → live); ok is false whenever there is no signal — ProcStart == 0 or
// the verdict-time read failed — and the caller must fall through to argv.
func startTimeVerdict(p Peer) (v LivenessVerdict, ok bool) {
	if p.ProcStart == 0 {
		return LivenessVerdict{}, false
	}
	cur, readOK := procStartFn(p.PID)
	if !readOK {
		// Unreadable proc table / unsupported platform → NO SIGNAL. Never dead.
		return LivenessVerdict{}, false
	}
	if cur != p.ProcStart {
		return LivenessVerdict{Liveness: SessionDead, Reason: "procstart-mismatch", Peer: &p}, true
	}
	return LivenessVerdict{Liveness: SessionLive, Reason: "socket+procstart", Peer: &p}, true
}

// mismatch reports whether a flag recorded non-empty no longer matches the
// occupant's argv. A flag that was never recorded ("") carries no signal —
// top-level sessions have neither --agent-id nor --parent-session-id, and an
// absent-vs-absent comparison must not fail them.
func mismatch(argv, flag, recorded string) bool {
	return recorded != "" && argvFlag(argv, flag) != recorded
}

// AgentLive answers "is the session behind this agent uuid still up?" from
// this host's peer registry. No record → SessionUnknown: the caller keeps its
// own degraded signal and must not reclaim/kill on that answer alone.
//
// Kill-class consumers (the bdx team-reaper): a LIVE verdict vetoes the kill,
// but a DEAD verdict is necessary, never sufficient — killing additionally
// requires an idle/TaskStop signal from the harness. Liveness says "the
// process is gone", not "the work is done".
func AgentLive(ctx context.Context, uuid string) LivenessVerdict {
	p, err := peerByUUID(uuid)
	if err != nil || p == nil {
		return LivenessVerdict{Liveness: SessionUnknown, Reason: "no-peer-record"}
	}
	return p.SessionVerdict(ctx)
}

// peerByUUID reads one peer record raw — no pruning side effects, unlike
// Peers(). The oracle must be a pure observer: a liveness *question* asked
// mid-race (session restarting, record being rewritten) must not unlink the
// fresh record the way the gj1z race does.
func peerByUUID(uuid string) (*Peer, error) {
	path, err := peerPath(uuid)
	if err != nil {
		return nil, err
	}
	p, ok := readPeerRaw(path)
	if !ok {
		return nil, nil //nolint:nilnil // "no record" is the normal SessionUnknown case, not an error
	}
	return &p, nil
}
