package domain

import (
	"testing"
	"time"
)

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	live := func(string, int, int64) bool { return true }
	dead := func(string, int, int64) bool { return false }

	t.Run("past TTL is stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: live}
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: "h", PID: 1}
		if !ctx.IsStale(l) {
			t.Fatal("past TTL must be stale")
		}
	})
	t.Run("dead pid same host is stale even when TTL not reached", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: dead}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if !ctx.IsStale(l) {
			t.Fatal("dead pid on same host must be stale")
		}
	})
	t.Run("dead pid other host is NOT stale (out of scope)", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "this", Live: dead}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "other", PID: 1}
		if ctx.IsStale(l) {
			t.Fatal("dead pid on other host must not stale-flag")
		}
	})
	t.Run("live and within TTL is not stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: live}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1}
		if ctx.IsStale(l) {
			t.Fatal("live within TTL must not be stale")
		}
	})

	// PID-reuse recycle protection (loto-kwlp). The probe receives the lock's
	// stored start-time and may report a pid dead when the current occupant's
	// start-time differs — even though Kill(pid,0) alone would say "alive".
	recycleAware := func(_ string, _ int, storedStart int64) bool {
		const currentStart = 5000
		if storedStart != 0 && storedStart != currentStart {
			return false // recycled: pid alive but not our original holder
		}
		return true
	}

	t.Run("recycled pid (stored start differs from occupant) is stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 4000}
		if !ctx.IsStale(l) {
			t.Fatal("recycled pid (start mismatch) must be stale")
		}
	})
	t.Run("matching start-time is not stale", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 5000}
		if ctx.IsStale(l) {
			t.Fatal("matching start-time must not be stale")
		}
	})
	t.Run("legacy row (ProcStart 0) falls back to pid-alive-only", func(t *testing.T) {
		ctx := EvalContext{Now: now, ThisHost: "h", Live: recycleAware}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, ProcStart: 0}
		if ctx.IsStale(l) {
			t.Fatal("unknown start-time (legacy) must fall back to pid-alive: not stale when alive")
		}
	})
}

// TestIsStale_NoDurablePid covers the PID-0 sentinel: a lock placed without a
// durable liveness pid (LOTO_PID unset → loto-t1tq/loto-j1bo). Liveness is
// unknown, so the TTL lease alone governs — the dead-pid branch must never fire,
// and the probe must not even be consulted.
func TestIsStale_NoDurablePid(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	probeCalled := false
	probe := func(string, int, int64) bool { probeCalled = true; return false }
	ctx := EvalContext{Now: now, ThisHost: "h", Live: probe}

	t.Run("PID 0 within TTL is not stale and never probes liveness", func(t *testing.T) {
		probeCalled = false
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "h", PID: 0}
		if ctx.IsStale(l) {
			t.Fatal("PID-0 lock within TTL must not be stale (TTL-only liveness)")
		}
		if probeCalled {
			t.Fatal("liveness probe must not be consulted for a PID-0 lock")
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
	aliveProbe := func(string, int, int64) bool { return true }
	deadProbe := func(string, int, int64) bool { return false }

	t.Run("ALIVE: durable pid, probe live, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessAlive {
			t.Errorf("Classify=%v want ALIVE", got)
		}
		if got := ec.RemainingTTL(l); got != time.Hour {
			t.Errorf("RemainingTTL=%v want 1h", got)
		}
	})
	t.Run("DEAD by dead probe, TTL still ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: deadProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
	})
	t.Run("DEAD by expired TTL even if probe live", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(-time.Minute), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
		if got := ec.RemainingTTL(l); got != 0 {
			t.Errorf("RemainingTTL=%v want 0 (clamped)", got)
		}
	})
	t.Run("UNKNOWN: PID-0 sentinel, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 0}
		if got := ec.Classify(l); got != LivenessUnknown {
			t.Errorf("Classify=%v want UNKNOWN", got)
		}
	})
	t.Run("UNKNOWN: cross-host holder, TTL ahead", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := LockRecord{ExpiresAt: now.Add(time.Hour), Host: "other-host", PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != LivenessUnknown {
			t.Errorf("Classify=%v want UNKNOWN (cross-host, no probe)", got)
		}
	})
	t.Run("Classify=DEAD iff IsStale (invariant I1)", func(t *testing.T) {
		ec := EvalContext{Now: now, ThisHost: host, Live: deadProbe}
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
