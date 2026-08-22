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

// IsBeacon is what lets `loto guard` tell "an agent of my own session is
// writing here" apart from "a peer holds this territory" (loto-xwod). The
// marker is persisted, never inferred: the shared/pid-0 shape a beacon wears is
// also the shape of an ordinary `loto lock --shared` placed without LOTO_PID,
// and reading the shape let guard waive that real lease and move the tree
// (loto-dm4i, Codex #249). The pid-0 shared row below is the regression pin.
func TestLockRecordIsBeacon(t *testing.T) {
	withPID := func(l LockRecord, pid int) LockRecord { l.PID = pid; return l }
	asBeacon := func(l LockRecord) LockRecord { l.Beacon = true; return l }
	cases := []struct {
		name string
		l    LockRecord
		want bool
	}{
		{"marked beacon", asBeacon(mk("alice", ModeShared)), true},
		{"shared + pid 0 unmarked is a pid-less shared lock", mk("alice", ModeShared), false},
		{"shared + live pid is a held shared lock", withPID(mk("alice", ModeShared), 4242), false},
		{"exclusive + pid 0", mk("alice", ModeExclusive), false},
		{"empty mode reads as exclusive", mk("alice", ""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.l.IsBeacon(); got != c.want {
				t.Fatalf("IsBeacon() = %v, want %v", got, c.want)
			}
		})
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

// Kin (loto-wofb): the parent identity behind a subagent stamp counts as self
// for conflicts — in either mode — while any other owner still conflicts.
func TestConflicts_KinNeverConflicts(t *testing.T) {
	ec := EvalContext{Now: time.Now(), Kin: []AgentUUID{"parent"}}
	cases := []struct {
		name string
		a, l LockRecord
		want bool
	}{
		{"kin excl vs incoming shared", mk("sib", ModeShared), mk("parent", ModeExclusive), false},
		{"kin excl vs incoming excl", mk("sib", ModeExclusive), mk("parent", ModeExclusive), false},
		{"non-kin excl still conflicts", mk("sib", ModeShared), mk("other", ModeExclusive), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ec.Conflicts(c.a, c.l); got != c.want {
				t.Fatalf("Conflicts = %v, want %v", got, c.want)
			}
		})
	}
}
