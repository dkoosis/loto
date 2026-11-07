package domain

import (
	"testing"
	"time"
)

func mk(owner string) LockRecord {
	return LockRecord{
		Target:    Target{Canonical: "/a.go"},
		OwnerUUID: AgentUUID(owner),
		ExpiresAt: time.Now().Add(time.Hour), // not stale
		PID:       0,                         // PID<=0 → never instant-stale
	}
}

// IsBeacon reads the persisted flag and nothing else (loto-dm4i): a pid-0
// row is not a beacon unless marked.
func TestLockRecordIsBeacon(t *testing.T) {
	l := mk("alice")
	if l.IsBeacon() {
		t.Fatal("an unmarked pid-0 row is not a beacon")
	}
	l.Beacon = true
	if !l.IsBeacon() {
		t.Fatal("a marked row is a beacon")
	}
}

func TestConflicts_TruthTable(t *testing.T) {
	ec := EvalContext{Now: time.Now()}
	cases := []struct {
		name string
		a, l LockRecord
		want bool
	}{
		{"diff owner conflicts", mk("alice"), mk("bob"), true},
		{"same owner never conflicts", mk("alice"), mk("alice"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ec.Conflicts(c.a, c.l); got != c.want {
				t.Fatalf("Conflicts = %v, want %v", got, c.want)
			}
		})
	}
}

// Conflicts probes a holder's liveness only for a row that could conflict
// (loto-kxxs): none for a same-owner row or one whose target does not cover.
func TestConflicts_ProbesOnlyRowsThatCouldConflict(t *testing.T) {
	probes := 0
	ec := EvalContext{Now: time.Now(), Live: func(LockRecord) Liveness { probes++; return LivenessAlive }}
	in := mk("alice")
	other := mk("bob")
	other.Target = Target{Canonical: "/b.go"}
	if ec.Conflicts(in, mk("alice")) || ec.Conflicts(in, other) {
		t.Fatal("same-owner and non-covering rows must not conflict")
	}
	if probes != 0 {
		t.Fatalf("probe called %d times for rows that cannot conflict, want 0", probes)
	}
	if !ec.Conflicts(in, mk("bob")) || probes != 1 {
		t.Fatalf("foreign covering row must conflict after one probe, probes=%d", probes)
	}
}
