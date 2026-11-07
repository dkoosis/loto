package domain

import (
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
	// CaseFold is true when this repo's checkout is on a case-folding
	// filesystem, and it widens SameTarget/PrefixCovers (and so Refuses) to
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
	// MyWorktree is the caller's own checkout key (rt.Worktree), for a
	// decision surface that scans lock rows directly rather than building an
	// incoming LockRecord to hand to Conflicts (gateDecide,
	// hookRowsDecide — loto-3eq6). Compared against a row's LockRecord.Worktree
	// via SameWorktree. Zero value "" widens rather than narrows, matching
	// every other ambient field here.
	MyWorktree string
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
	return c.Live(l)
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
// holder `existing`: Refuses, the one conflict rule. Same-owner holders never conflict (re-acquire is an upsert), and a
// stale holder never does (loto-k5el.2).
func (c EvalContext) Conflicts(incoming, existing LockRecord) bool {
	// Probe liveness (LockHold) only for a row that could conflict: foreign and
	// covering. Refuses with Live true is that cheap filter (loto-kxxs).
	cheap := Hold{Actor: existing.OwnerUUID, Worktree: existing.Worktree, Target: existing.Target.Canonical, Live: true}
	if !c.Refuses(incoming.OwnerUUID, incoming.Worktree, incoming.Target.Canonical, cheap) {
		return false
	}
	return c.Refuses(incoming.OwnerUUID, incoming.Worktree, incoming.Target.Canonical, c.LockHold(existing))
}
