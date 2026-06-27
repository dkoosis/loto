package domain

import "time"

// AgentUUID is the stable per-agent identity that owns locks and tags. It is a
// distinct named type (not a bare string) so that transposing it with an
// adjacent untyped string argument — e.g. a session UUID, a target path, an
// intent — is a compile error rather than a silent runtime bug (loto-inf4,
// stage 1 of loto-34n3). Values cross untyped edges (env vars, CLI flags,
// sqlite columns) via explicit AgentUUID(s)/string(x) conversions. The
// identity package cannot reference this type — the arch layering pins
// internal/identity → ∅ — so an identity-sourced UUID is converted to
// AgentUUID at the cli boundary.
type AgentUUID string

// SessionUUID is the per-session identity (one Claude session = one id, shared
// by every shell-out from that session; sourced from LOTO_SESSION_ID). It is a
// distinct named type from AgentUUID so the runtime.SessionUUID-vs-Agent.UUID
// overload — the original swap hazard — and ReleaseBySession(agent, session)
// can no longer transpose silently (loto-ww4x, stage 2 of loto-34n3). Values
// cross untyped edges (env var, sqlite session_uuid column) via explicit
// SessionUUID(s)/string(x) conversions.
type SessionUUID string

// Canonical is a canonicalized, repo-relative target path — the cleaned form
// minted by Canonicalize and carried in Target.Canonical. It is the path member
// of the swap-safe identity family (loto-ip6c, stage 3 of loto-34n3): a distinct
// named type so a path can't silently transpose with an adjacent bare-string
// field — e.g. NewTag{TargetCanonical, TaggerUUID} can no longer be filled in the
// wrong order and still compile. Scoped to the tag surface (Tag/NewTag plus
// ListAliveForTarget/ListAliveByTargets) where the path sits beside other
// strings; Target.Canonical stays a bare string (the persisted-form home and an
// untyped edge), so values cross via explicit Canonical(s)/string(x) conversions
// at that boundary, the sqlite target_canonical column, and CLI callers.
type Canonical string

type LockRecord struct {
	Target      Target
	OwnerUUID   AgentUUID
	SessionUUID SessionUUID
	Intent      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Host        string
	PID         int
	// ProcStart is the holder process's OS start-time, read at acquire on the
	// local host. It defeats PID reuse: if the original holder dies and the OS
	// recycles its PID to an unrelated process, the recycled occupant's
	// start-time won't match, so the lock is correctly treated as stale
	// (loto-kwlp). The encoding is opaque and per-OS — it is only ever compared
	// for equality on the same host/OS (IsStale probes liveness on
	// l.Host == thisHost only). Zero means UNKNOWN: legacy rows predating this
	// field, or OSes where start-time can't be read. Unknown falls back to
	// pid-alive-only (today's behavior).
	ProcStart int64
	Branch    string
	// Mode is the lease mode: ModeShared (multi-reader, advisory only, write-bit
	// NOT stripped) or ModeExclusive (sole-writer, write-bit stripped). Empty
	// string reads as exclusive — preserves the pre-mode binary-lock semantics
	// for legacy rows (loto-k5el.2). Normalize via EffectiveMode().
	Mode string
}

const (
	ModeShared    = "shared"
	ModeExclusive = "exclusive"
)

// EffectiveMode normalizes a possibly-empty Mode to exclusive (legacy default).
func (l LockRecord) EffectiveMode() string {
	if l.Mode == ModeShared {
		return ModeShared
	}
	return ModeExclusive // empty or any non-"shared" value → exclusive
}

// Event is an append-only audit row. SubjectUUID is the affected agent (for
// lock_broken / lock_reclaimed_stale); empty otherwise.
type Event struct {
	ID          string
	Target      Target
	Kind        string
	ActorUUID   string
	SubjectUUID string
	Reason      string
	CreatedAt   time.Time
}
