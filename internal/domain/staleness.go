package domain

import "time"

// PidLiveProbe returns true if (host,pid) is currently running. storedStart is
// the lock's persisted holder start-time (0 = unknown): when nonzero, the real
// probe reads the current occupant's start-time and reports the pid dead on a
// mismatch, defeating PID reuse (loto-kwlp). Unknown (0) degrades to a plain
// pid-alive check.
type PidLiveProbe func(host string, pid int, storedStart int64) bool

// EvalContext bundles the ambient inputs every staleness/authz predicate needs:
// the evaluation clock, the host doing the evaluating, and the pid-liveness
// probe. It replaces the (now, thisHost, live) trio that previously threaded
// positionally through IsStale/AuthorizeBreak and their call sites — a real
// arg-order hazard given two strings-and-a-func with no compiler guard
// (loto-vtg6). The LockRecord stays the per-call subject and is passed
// separately.
type EvalContext struct {
	Now      time.Time
	ThisHost string
	Live     PidLiveProbe
}

// WithHost returns a copy of the context evaluating from host. Acquisition
// reclaim scans blockers from the perspective of the acquiring lock's host, so
// the same (now, live) ambient pair is reused while ThisHost varies per lock.
func (c EvalContext) WithHost(host string) EvalContext {
	c.ThisHost = host
	return c
}

// IsStale returns true if the lock is past its TTL OR the holder pid is provably
// dead on this host. Cross-host pid checks are out of scope.
func (c EvalContext) IsStale(l LockRecord) bool {
	if !c.Now.Before(l.ExpiresAt) {
		return true
	}
	// PID <= 0 is the no-durable-liveness sentinel (a lock placed without
	// LOTO_PID — loto-t1tq/loto-j1bo): the holder pid is unknown, so liveness
	// can't be probed and the TTL gate above is the sole authority. Never
	// instant-stale, never consult the probe. A real holder pid (>0) does.
	// A nil probe (zero-value EvalContext) is the same "undeterminable" case →
	// TTL governs, no panic.
	if l.PID > 0 && l.Host == c.ThisHost && c.Live != nil && !c.Live(l.Host, l.PID, l.ProcStart) {
		return true
	}
	return false
}

// Liveness is the display-tier refinement of IsStale: it splits a non-stale
// lock into ALIVE (owner session probed live) vs UNKNOWN (no durable liveness
// handle — PID-0 sentinel or cross-host — so TTL is the sole authority). DEAD
// is exactly IsStale: TTL backstop fired OR owner provably gone. Surfaced by
// `loto status` so the cause of a lock's verdict is visible (loto-k5el.1).
type Liveness int

const (
	LivenessAlive Liveness = iota
	LivenessDead
	LivenessUnknown
)

func (l Liveness) String() string {
	switch l {
	case LivenessAlive:
		return "alive"
	case LivenessDead:
		return "dead"
	case LivenessUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Classify returns the display-tier liveness verdict. DEAD ⟺ IsStale (I1).
func (c EvalContext) Classify(l LockRecord) Liveness {
	if c.IsStale(l) {
		return LivenessDead
	}
	if l.PID > 0 && l.Host == c.ThisHost && c.Live != nil {
		// Not stale + durable handle on this host ⟹ probe said alive.
		return LivenessAlive
	}
	return LivenessUnknown
}

// RemainingTTL is the time until the TTL backstop fires, clamped at 0. A live
// durable-PID holder is never TTL-reaped (liveness governs), so this is purely
// informational for ALIVE locks; for UNKNOWN locks it is the self-heal deadline.
func (c EvalContext) RemainingTTL(l LockRecord) time.Duration {
	d := l.ExpiresAt.Sub(c.Now)
	if d < 0 {
		return 0
	}
	return d
}

// Conflicts reports whether an incoming acquire `incoming` is blocked by existing
// holder `existing`. Shared+shared on the same target coexist; an exclusive lease on
// either side conflicts. Same-owner holders never conflict (re-acquire is an
// upsert). A stale holder never conflicts — the caller is expected to have
// reclaimed it, but this guards the predicate independently (loto-k5el.2).
func (c EvalContext) Conflicts(incoming, existing LockRecord) bool {
	if existing.OwnerUUID == incoming.OwnerUUID {
		return false
	}
	if !SameCanonical(incoming.Target, existing.Target) {
		return false
	}
	if c.IsStale(existing) {
		return false
	}
	return incoming.EffectiveMode() == ModeExclusive || existing.EffectiveMode() == ModeExclusive
}
