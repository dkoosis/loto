package domain

import (
	"testing"
	"time"
)

func mk(owner, mode string) LockRecord {
	return LockRecord{
		Target:    Target{Canonical: "/a.go"},
		OwnerUUID: AgentUUID(owner),
		Mode:      mode,
		ExpiresAt: time.Now().Add(time.Hour), // not stale
		PID:       0,                         // PID<=0 → never instant-stale
	}
}

func TestConflicts_TruthTable(t *testing.T) {
	ec := EvalContext{Now: time.Now()}
	cases := []struct {
		name string
		a, l LockRecord
		want bool
	}{
		{"shared+shared diff owner", mk("alice", ModeShared), mk("bob", ModeShared), false},
		{"shared+excl   diff owner", mk("alice", ModeShared), mk("bob", ModeExclusive), true},
		{"excl+shared   diff owner", mk("alice", ModeExclusive), mk("bob", ModeShared), true},
		{"excl+excl     diff owner", mk("alice", ModeExclusive), mk("bob", ModeExclusive), true},
		{"same owner never conflicts", mk("alice", ModeExclusive), mk("alice", ModeExclusive), false},
		{"empty mode reads as exclusive", mk("alice", ""), mk("bob", ModeShared), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ec.Conflicts(c.a, c.l); got != c.want {
				t.Fatalf("Conflicts(%s,%s) = %v, want %v", c.a.Mode, c.l.Mode, got, c.want)
			}
		})
	}
}
