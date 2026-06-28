package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestLocksForOwnerAt_BatchedMatchesPointQueries pins the batched owner-scoped
// query (loto-89n3): over a mixed target set it returns exactly owner's rows,
// keyed by canonical path, and omits both unheld targets and targets held by a
// DIFFERENT owner — so a caller reading a missing entry reproduces
// LockForOwnerAt's (nil,nil) "no row" verdict.
func TestLocksForOwnerAt_BatchedMatchesPointQueries(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	b := mkFileLock(t, "b.go", tcAlice, time.Hour)
	c := mkFileLock(t, "c.go", tcBob, time.Hour) // bob's, not alice's
	d := mkFileLock(t, "d.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b}, liveProbe); err != nil {
		t.Fatalf("alice acquire a,b: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{c}, liveProbe); err != nil {
		t.Fatalf("bob acquire c: %v", err)
	}
	// d is never acquired → unheld.

	targets := []domain.Target{a.Target, b.Target, c.Target, d.Target}
	got, err := s.LocksForOwnerAt(ctx, targets, tcAlice)
	if err != nil {
		t.Fatalf("LocksForOwnerAt: %v", err)
	}

	// alice's a,b present; bob's c and unheld d absent.
	want := map[string]bool{a.Target.Canonical: true, b.Target.Canonical: true}
	for canon := range want {
		l := got[canon]
		if l == nil {
			t.Errorf("target %s: want alice's lock, got absent", canon)
			continue
		}
		if l.OwnerUUID != tcAlice {
			t.Errorf("target %s: owner=%s, want alice", canon, l.OwnerUUID)
		}
	}
	if l := got[c.Target.Canonical]; l != nil {
		t.Errorf("target c held by bob must be absent for alice, got %+v", l)
	}
	if l := got[d.Target.Canonical]; l != nil {
		t.Errorf("unheld target d must be absent, got %+v", l)
	}
	if len(got) != len(want) {
		t.Errorf("map size = %d, want %d (only alice's held targets)", len(got), len(want))
	}

	// Each batched record must equal the point query it replaces.
	for canon := range want {
		pt, err := s.LockForOwnerAt(ctx, domain.Target{Canonical: canon}, tcAlice)
		if err != nil || pt == nil {
			t.Fatalf("LockForOwnerAt(%s): %v / nil", canon, err)
		}
		if !got[canon].CreatedAt.Equal(pt.CreatedAt) || got[canon].OwnerUUID != pt.OwnerUUID {
			t.Errorf("target %s: batched %+v != point %+v", canon, got[canon], pt)
		}
	}
}

// TestLocksForOwnerAt_EmptyTargets guards the IN()-syntax edge: zero targets
// must short-circuit to an empty (non-nil) map, never an empty SQL IN clause.
func TestLocksForOwnerAt_EmptyTargets(t *testing.T) {
	s := mustOpen(t)
	got, err := s.LocksForOwnerAt(context.Background(), nil, tcAlice)
	if err != nil {
		t.Fatalf("LocksForOwnerAt(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("want empty non-nil map, got %v", got)
	}
}
