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
	Reason   string // machine-stable token, e.g. "socket+ps-match", "socket-missing"
	Peer     *Peer  // the record consulted; nil when none exists
}

// SessionVerdict is the oracle's decision procedure over one peer record:
//
//	socket missing            → dead   (the session process removes it by dying)
//	socket pid not running    → dead   (orphaned socket file)
//	argv identity mismatch    → dead   (OS recycled the pid to another process)
//	otherwise                 → live
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
	argv := procArgv(ctx, p.PID)
	if argv == "" {
		return LivenessVerdict{Liveness: SessionLive, Reason: "socket+pid", Peer: &p}
	}
	if mismatch(argv, "agent-id", p.AgentID) || mismatch(argv, "parent-session-id", p.ParentSessionID) {
		return LivenessVerdict{Liveness: SessionDead, Reason: "argv-mismatch", Peer: &p}
	}
	return LivenessVerdict{Liveness: SessionLive, Reason: "socket+ps-match", Peer: &p}
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
