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
	// CaseFold is true when this repo's checkout is on a case-folding
	// filesystem, and it widens SameTarget/PrefixCovers/ClaimCovers below to
	// compare keys ignoring case (loto-8soe). The CLI mints every new key
	// already folded (cli.foldTargetKey), so this changes nothing for rows
	// written by this binary — it is what lets a row minted by an OLDER loto,
	// carrying the on-disk spelling, keep resolving against the folded key a
	// caller now presents. Default false = today's byte comparison, which is
	// also the correct and permanent answer on a case-sensitive filesystem.
	//
	// ‡ The rule for a NEW EvalContext: every context that reaches a path
	// comparison must set this from the store (store.Store.caseFold), and a
	// context built only to answer liveness questions — Classify, IsStale,
	// CandidateClaimIsDead over rows nobody looks up by name, as in
	// gate.reclaimDeadPromotions — may leave it zero. Zero is not "unknown", it
	// is "compare byte-exact", so the omission can never widen a decision by
	// accident; it can only fail to widen one, which leaves a legacy row
	// blocking rather than admitting a peer.
	CaseFold bool
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

// HolderLiveness reports the probe's RAW verdict on this lock's holder, and
// whether there was a probe to ask. It answers the question IsStale deliberately
// never asks: what is the holder doing RIGHT NOW, whatever its TTL says?
//
// IsStale short-circuits on expiry, which is correct for reclaiming a lock ROW —
// lapsing frees the territory, and refresh is the remedy. It is not correct for
// destroying the holder's uncommitted bytes on disk, so `loto sync` asks this
// second question before it overwrites a lapsed path (loto-0o0j).
//
// ‡ The caller must not fold UNKNOWN into DEAD. Unknown is the answer for a
// remote host, a PID-0 sentinel, a peer record that cannot be read — it means
// "no witness", not "provably gone", and an agent on another host holding a
// lapsed lease is exactly as capable of losing its edits as one on this host.
// Only DEAD authorizes the overwrite.
//
// probed=false is the zero-value EvalContext with no probe at all: no question
// was asked, so TTL stays the sole authority and every existing caller's
// behaviour is unchanged.
func (c EvalContext) HolderLiveness(l LockRecord) (v Liveness, probed bool) {
	if c.Live == nil {
		return LivenessUnknown, false
	}
	return c.Live(l), true
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
	return c.IsStale(claimAsLock(cl))
}

// claimAsLock lifts a claim into the lock shape the probe and the staleness
// predicates take as their subject. The PID-0 sentinel is deliberate: a claim
// carries no PID and no start time, so the pid fallback must read UNKNOWN and
// leave the uuid-keyed session oracle as the only witness.
func claimAsLock(cl ClaimRecord) LockRecord {
	return LockRecord{
		OwnerUUID:   cl.OwnerUUID,
		SessionUUID: cl.SessionUUID,
		Host:        cl.Host,
		ExpiresAt:   cl.ExpiresAt,
	}
}

// ClaimHolderIsLive reports whether the claim's owner is PROVABLY running now,
// whatever the claim's TTL says. It is HolderLiveness for claims, narrowed to
// the one verdict that is safe to act on here (loto-3dhl).
//
// ‡ The narrowing is the design, and it is why this is not simply
// HolderLiveness(claimAsLock(cl)). A lock row carries a PID and a start time,
// so its probe has a local fallback and can return DEAD; refusing an overwrite
// on UNKNOWN there costs one operator glance at another host and still
// self-heals. A claim carries neither, so a claim whose session record has
// aged out probes UNKNOWN forever. Refusing on UNKNOWN would let one crashed
// agent's lapsed claim block `loto sync` across its whole prefix with no
// reclaim path — the freeze ClaimIsStale's comment above was written to end.
// Only ALIVE, a positive witness from the session oracle, holds the bytes back.
func (c EvalContext) ClaimHolderIsLive(cl ClaimRecord) bool {
	v, probed := c.HolderLiveness(claimAsLock(cl))
	return probed && v == LivenessAlive
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
	if !c.SameTarget(incoming.Target, existing.Target) {
		return false
	}
	if c.IsStale(existing) {
		return false
	}
	return incoming.EffectiveMode() == ModeExclusive || existing.EffectiveMode() == ModeExclusive
}
