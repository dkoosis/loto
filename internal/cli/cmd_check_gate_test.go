package cli

import (
	"testing"
	"time"

	"loto/internal/domain"
)

// gateDecide unit tests (loto-vr2, gate-design.md component 4). Pure —
// domain.LockRecord/domain.ClaimRecord fixtures in, []gateDeny out — no
// store, no CLI, no clock read beyond the EvalContext/now passed in. These
// pin the deliberate divergences from plain check's computeCheckConflicts:
// any-mode foreign-lock deny, claims consulted at all, and !IsStale (not
// Classify==Alive) as the liveness threshold.

const (
	gateMyUUID  = "11111111-1111-1111-1111-111111111111"
	gateFoeUUID = "22222222-2222-2222-2222-222222222222"
)

func gateEC(now time.Time) domain.EvalContext {
	return domain.EvalContext{Now: now, ThisHost: "host-a", Live: nil}
}

func TestGateDecide_ForeignLiveClaimDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"} // kuv.10 class: not on disk
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "refactor", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 1 {
		t.Fatalf("want 1 deny row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Kind != gateKindClaim || rows[0].HolderUUID != gateFoeUUID || rows[0].Path != target.Canonical {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}

func TestGateDecide_OwnClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateMyUUID, Intent: "mine", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own claim must not deny, got %+v", rows)
	}
}

func TestGateDecide_ExpiredClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "stale", ExpiresAt: now.Add(-time.Minute)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("expired claim must not deny, got %+v", rows)
	}
}

func TestGateDecide_ForeignSharedBeaconDenies(t *testing.T) {
	// Any-mode divergence from plain check: a foreign SHARED lock still denies
	// under the gate (plan point 1) — plain check's shared-vs-shared probe
	// would pass this.
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, Intent: "beacon", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].Kind != gateKindLock || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("want 1 lock-kind deny row, got %+v", rows)
	}
}

func TestGateDecide_ForeignStaleLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, Intent: "old", ExpiresAt: now.Add(-time.Minute)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("stale (TTL-lapsed) lock must not deny, got %+v", rows)
	}
}

func TestGateDecide_OwnLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, Mode: domain.ModeExclusive, Intent: "mine", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own lock must not deny, got %+v", rows)
	}
}

// TestGateDecide_PID0ForeignBeaconWithinTTLDenies pins the liveness-threshold
// divergence (plan point 3): the gate uses !IsStale, not Classify==Alive, so
// a hook-minted beacon with no durable PID (the sentinel PID<=0 — no
// LOTO_PID) still denies while its TTL hasn't lapsed. Classify would report
// this holder LivenessUnknown; plain check's Classify==Alive gate would only
// warn, never hard-block, on this row.
func TestGateDecide_PID0ForeignBeaconWithinTTLDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, PID: 0, Intent: "beacon", ExpiresAt: now.Add(time.Hour)},
	}
	ec := gateEC(now)
	if ec.Classify(locks[0]) != domain.LivenessUnknown {
		t.Fatalf("test fixture invariant broken: want LivenessUnknown, got %v", ec.Classify(locks[0]))
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, ec)
	if len(rows) != 1 || rows[0].Kind != gateKindLock {
		t.Fatalf("PID-0 beacon within TTL must deny, got %+v", rows)
	}
}

// TestGateDecide_DedupeAndOrder: a claim AND a lock both cover the same
// path from the same foreign owner must not collapse into one row (distinct
// kinds), a duplicate target in the input must not double-count, and output
// is sorted path -> kind -> holder UUID regardless of input order.
func TestGateDecide_DedupeAndOrder(t *testing.T) {
	now := time.Now()
	tB := domain.Target{Canonical: "b.go"}
	tA := domain.Target{Canonical: "internal/store/a.go"}
	locks := []domain.LockRecord{
		{Target: tB, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, Intent: "lockrow", ExpiresAt: now.Add(time.Hour)},
	}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "claimrow", ExpiresAt: now.Add(time.Hour)},
	}
	// tB passed twice (duplicate CLI arg) + tA once, out of sorted order.
	rows := gateDecide([]domain.Target{tB, tA, tB}, locks, claims, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 deduped rows (one per distinct path), got %d: %+v", len(rows), rows)
	}
	// path "b.go" < "internal/store/a.go" is false lexicographically ('b' >
	// 'i'? no: 'b'=0x62, 'i'=0x69, so "b.go" < "internal/store/a.go" is true).
	if rows[0].Path != "b.go" || rows[1].Path != "internal/store/a.go" {
		t.Fatalf("want path-sorted rows, got %+v", rows)
	}
	if rows[0].Kind != gateKindLock || rows[1].Kind != gateKindClaim {
		t.Fatalf("unexpected kinds: %+v", rows)
	}
}
