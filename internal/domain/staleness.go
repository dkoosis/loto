package domain

import (
	"slices"
	"time"
)

// HolderLiveProbe answers "is the holder of this lock still alive?" with the
// display-tier trichotomy. The probe owns ALL environmental policy — which
// host it runs on, the session-liveness oracle (identity.AgentLive, loto-ygty),
// the pid + start-time fallback (loto-kwlp) — so the domain predicates stay
// pure over the verdict. Replaces PidLiveProbe(host, pid, storedStart): the
// record is the subject, and a positional (string, int, int64) probe cannot
// carry the owner uuid the oracle keys on.
//
// Contract: LivenessAlive = holder provably up (oracle or local pid probe);
// LivenessDead = provably gone; LivenessUnknown = no durable handle (remote
// host, PID-0 sentinel, no peer record) → TTL is the sole authority.
type HolderLiveProbe func(l LockRecord) Liveness

// EvalContext bundles the ambient inputs every staleness/authz predicate needs:
// the evaluation clock and the holder-liveness probe. It replaces the trio that
// previously threaded positionally through IsStale/AuthorizeBreak and their
// call sites — a real arg-order hazard with no compiler guard (loto-vtg6). The
// LockRecord stays the per-call subject and is passed separately.
type EvalContext struct {
	Now  time.Time
	Live HolderLiveProbe
	// Kin are owner UUIDs whose rows count as the caller's OWN for conflict
	// purposes: today, the parent identity a LOTO_SUBAGENT_ID stamp hides
	// (identity.EnsureParent, loto-wofb). A stamped sibling's Bash-side
	// `loto lock`/`claim` rows are owned by that parent — hooks cannot export
	// env into later tool calls — so a beacon or gate check that read them as
	// foreign would refuse a worker on the territory it just took. A SIBLING's
	// rows are never kin: distinct stamp, distinct uuid, and serializing those
	// is the point of stamping (loto-xwod). Empty = only exact-owner is self.
	Kin []AgentUUID
}

// IsKin reports whether owner u counts as the caller's own for conflicts.
func (c EvalContext) IsKin(u AgentUUID) bool {
	return slices.Contains(c.Kin, u)
}

// IsStale returns true if the lock is past its TTL OR the holder is provably
// dead. A nil probe (zero-value EvalContext) means liveness is undeterminable →
// TTL governs, no panic. Liveness accelerates staleness, it never extends the
// lease: an ALIVE holder past its TTL is still stale (refresh is the remedy —
// locks_refresh.go).
func (c EvalContext) IsStale(l LockRecord) bool {
	if !c.Now.Before(l.ExpiresAt) {
		return true
	}
	return c.Live != nil && c.Live(l) == LivenessDead
}

// ClaimIsStale applies the SAME staleness standard to a claim that IsStale
// applies to a lock: TTL lapse OR the owner provably dead (loto-tzmv.9).
// Claims carry no PID or start-time, so the subject handed to the probe is the
// claim lifted into lock shape — owner, session, host, expiry — with a PID-0
// sentinel. That keeps the probe's uuid-keyed session oracle in play while the
// pid fallback correctly reads UNKNOWN, so TTL remains the sole authority
// exactly when there is no better witness.
//
// This is deliberately NOT a second definition of "live": the claim gate
// previously filtered on Expired alone, so one crashed session's 2h claim
// froze every tree-move in the repo for its full lease with no reclaim path.
// One predicate, two record kinds — drift here is the bug.
func (c EvalContext) ClaimIsStale(cl ClaimRecord) bool {
	return c.IsStale(LockRecord{
		OwnerUUID:   cl.OwnerUUID,
		SessionUUID: cl.SessionUUID,
		Host:        cl.Host,
		ExpiresAt:   cl.ExpiresAt,
	})
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

// Classify returns the display-tier liveness verdict. DEAD ⟺ IsStale (I1):
// the TTL backstop firing renders the lock DEAD for display even when the
// holder still probes alive, because the lock is reclaimable either way.
func (c EvalContext) Classify(l LockRecord) Liveness {
	if c.IsStale(l) {
		return LivenessDead
	}
	if c.Live == nil {
		return LivenessUnknown
	}
	return c.Live(l) // not stale ⟹ probe returned Alive or Unknown
}

// RemainingTTL is the time until the TTL backstop fires, clamped at 0. Expiry
// is unconditional in IsStale — TTL lapse makes the lock reclaimable even when
// the holder pid still probes ALIVE (liveness accelerates staleness, it never
// extends the lease) — so this is every lock's hard self-heal deadline, not
// just the UNKNOWN ones'.
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
// upsert), and neither does a Kin holder (see EvalContext.Kin). A stale
// holder never conflicts — the caller is expected to have reclaimed it, but
// this guards the predicate independently (loto-k5el.2).
func (c EvalContext) Conflicts(incoming, existing LockRecord) bool {
	if existing.OwnerUUID == incoming.OwnerUUID || c.IsKin(existing.OwnerUUID) {
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
