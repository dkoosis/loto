package domain

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	live := aliveOn("h")
	dead := deadOn("h")

	t.Run("past TTL is stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: live}
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
		if !ctx.IsStale(l) {
			t.Fatal("past TTL must be stale")
		}
	})
	t.Run("dead pid same host is stale even when TTL not reached", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: dead}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if !ctx.IsStale(l) {
			t.Fatal("dead pid on same host must be stale")
		}
	})
	t.Run("dead pid other host is NOT stale (out of scope)", func(t *testing.T) {
		// Evaluating from "this": the holder sits on "other", so the probe has
		// no durable handle → UNKNOWN, and TTL alone governs.
		ctx := EvalContext{Now: now, Live: deadOn("this")}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "other", PID: 1}
		if ctx.IsStale(l) {
			t.Fatal("dead pid on other host must not stale-flag")
		}
	})
	t.Run("live and within TTL is not stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: live}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if ctx.IsStale(l) {
			t.Fatal("live within TTL must not be stale")
		}
	})

	// PID-reuse recycle protection (loto-kwlp). The probe sees the lock's stored
	// start-time and may report a pid DEAD when the current occupant's
	// start-time differs — even though Kill(pid,0) alone would say "alive".
	recycleAware := hostPidProbe("h", func(_ int, storedStart int64) bool {
		const currentStart = 5000
		if storedStart != 0 && storedStart != currentStart {
			return false // recycled: pid alive but not our original holder
		}
		return true
	})

	t.Run("recycled pid (stored start differs from occupant) is stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 4000}
		if !ctx.IsStale(l) {
			t.Fatal("recycled pid (start mismatch) must be stale")
		}
	})
	t.Run("matching start-time is not stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 5000}
		if ctx.IsStale(l) {
			t.Fatal("matching start-time must not be stale")
		}
	})
	t.Run("legacy row (ProcStart 0) falls back to pid-alive-only", func(t *testing.T) {
		ctx := EvalContext{Now: now, Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 0}
		if ctx.IsStale(l) {
			t.Fatal("unknown start-time (legacy) must fall back to pid-alive: not stale when alive")
		}
	})
}

// TestIsStale_NoDurablePid covers the PID-0 sentinel: a lock placed without a
// durable liveness pid (LOTO_PID unset → loto-t1tq/loto-j1bo). Liveness is
// unknown, so the TTL lease alone governs — the dead-pid branch must never fire,
// and the pid must not even be probed.
func TestIsStale_NoDurablePid(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	pidProbed := false
	probe := hostPidProbe("h", func(int, int64) bool { pidProbed = true; return false })
	ctx := EvalContext{Now: now, Live: probe}

	t.Run("PID 0 within TTL is not stale and never probes liveness", func(t *testing.T) {
		pidProbed = false
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 0}
		if ctx.IsStale(l) {
			t.Fatal("PID-0 lock within TTL must not be stale (TTL-only liveness)")
		}
		if pidProbed {
			t.Fatal("pid must not be probed for a PID-0 lock")
		}
	})
	t.Run("PID 0 past TTL is still stale", func(t *testing.T) {
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 0}
		if !ctx.IsStale(l) {
			t.Fatal("PID-0 lock past TTL must be stale (TTL gate still applies)")
		}
	})
}

// TestClassifyAndRemainingTTL pins loto-k5el.1 SC3 display helpers: Classify is
// the display-tier refinement of IsStale (DEAD ⟺ IsStale; splits ¬stale into
// ALIVE vs UNKNOWN) and RemainingTTL is the clamped TTL countdown.
//
// Package-local test (package domain) — types referenced unqualified.
func TestClassifyAndRemainingTTL(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	host := "h"
	aliveProbe := aliveOn(host)
	deadProbe := deadOn(host)

	t.Run("ALIVE: durable pid, probe live, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessAlive {
			t.Errorf("Classify=%v want ALIVE", got)
		}
		if got := ec.RemainingTTL(l); got != time.Hour {
			t.Errorf("RemainingTTL=%v want 1h", got)
		}
	})
	t.Run("DEAD by dead probe, TTL still ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: deadProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
	})
	t.Run("DEAD by expired TTL even if probe live", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
		if got := ec.RemainingTTL(l); got != 0 {
			t.Errorf("RemainingTTL=%v want 0 (clamped)", got)
		}
	})
	t.Run("UNKNOWN: PID-0 sentinel, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 0}
		if got := ec.Classify(l); got != LivenessUnknown {
			t.Errorf("Classify=%v want UNKNOWN", got)
		}
	})
	t.Run("UNKNOWN: cross-host holder, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "other-host", PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessUnknown {
			t.Errorf("Classify=%v want UNKNOWN (cross-host, no probe)", got)
		}
	})
	t.Run("Classify=DEAD iff IsStale (invariant I1)", func(t *testing.T) {
		ec := EvalContext{Now: now, Live: deadProbe}
		for _, l := range []LockRecord{
			{ExpiresAt: now.Add(-time.Minute), Host: host, PID: 1, ProcStart: 7},
			{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7},
			{ExpiresAt: now.Add(time.Hour), Host: host, PID: 0},
		} {
			if (ec.Classify(l) == LivenessDead) != ec.IsStale(l) {
				t.Errorf("I1 violated for %+v: Classify=%v IsStale=%v", l, ec.Classify(l), ec.IsStale(l))
			}
		}
	})
}

// ownerAliveProbe / ownerDeadProbe mirror the production probe's uuid-keyed
// session oracle, which answers for a PID-0 subject where the pid fallback
// cannot. Claims carry no PID, so a pid-gated stub (aliveOn/deadOn) would
// return UNKNOWN for every claim and silently skip the leg under test.
func ownerAliveProbe(LockRecord) Liveness { return LivenessAlive }
func ownerDeadProbe(LockRecord) Liveness  { return LivenessDead }

// TestClaimIsStale pins loto-tzmv.9: claims are judged by the same standard as
// locks — TTL lapse OR a provably dead owner — so a crashed session's claim
// stops denying immediately instead of freezing the repo for its full lease.
func TestClaimIsStale(t *testing.T) {
	now := time.Now()
	const host = "h1"
	cases := []struct {
		name  string
		claim ClaimRecord
		live  HolderLiveProbe
		want  bool
	}{
		{
			name:  "expired: TTL alone settles it",
			claim: ClaimRecord{ExpiresAt: now.Add(-time.Minute), Host: host},
			want:  true,
		},
		{
			name:  "unexpired, owner dead: liveness accelerates staleness",
			claim: ClaimRecord{ExpiresAt: now.Add(2 * time.Hour), Host: host},
			live:  ownerDeadProbe,
			want:  true,
		},
		{
			name:  "unexpired, owner alive: still held",
			claim: ClaimRecord{ExpiresAt: now.Add(2 * time.Hour), Host: host},
			live:  ownerAliveProbe,
			want:  false,
		},
		{
			name:  "unexpired, no probe: TTL is the sole authority",
			claim: ClaimRecord{ExpiresAt: now.Add(2 * time.Hour), Host: host},
			want:  false,
		},
		{
			name:  "expired, owner alive: liveness never extends the lease",
			claim: ClaimRecord{ExpiresAt: now.Add(-time.Minute), Host: host},
			live:  ownerAliveProbe,
			want:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ec := EvalContext{Now: now, Live: c.live}
			if got := ec.ClaimIsStale(c.claim); got != c.want {
				t.Errorf("ClaimIsStale(%+v) = %v, want %v", c.claim, got, c.want)
			}
		})
	}
}

// TestClaimIsStale_AgreesWithIsStale pins the "one predicate, two record
// kinds" rule: for the fields a claim carries, its verdict must equal the lock
// verdict on the same owner/host/expiry. A second definition of "live"
// drifting in is exactly the bug tzmv.9 fixed.
func TestClaimIsStale_AgreesWithIsStale(t *testing.T) {
	now := time.Now()
	const host = "h1"
	for _, probe := range []HolderLiveProbe{nil, ownerAliveProbe, ownerDeadProbe} {
		for _, exp := range []time.Time{now.Add(-time.Minute), now.Add(time.Hour)} {
			ec := EvalContext{Now: now, Live: probe}
			claim := ClaimRecord{OwnerUUID: "owner-1", Host: host, ExpiresAt: exp}
			lock := LockRecord{OwnerUUID: "owner-1", Host: host, ExpiresAt: exp}
			if got, want := ec.ClaimIsStale(claim), ec.IsStale(lock); got != want {
				t.Errorf("ClaimIsStale=%v but IsStale=%v for exp=%v", got, want, exp)
			}
		}
	}
}
