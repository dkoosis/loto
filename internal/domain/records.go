package domain

import "time"

type LockRecord struct {
	Target      Target
	OwnerUUID   string
	SessionUUID string
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
