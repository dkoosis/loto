package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

// loto-3eq6 follow-through (PR #372 review): the store is shared by every
// linked worktree of one repo, so a row stamped with a different Worktree is a
// different file on disk. Once two worktrees may each hold the same canonical,
// no mutation issued from one checkout may reach the other's row.

// TestBreakLocks_ForceDoesNotCrossWorktree: `unlock --force` in worktree A
// must not delete a live lease bob holds on the same canonical in worktree B.
func TestBreakLocks_ForceDoesNotCrossWorktree(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	s := mustOpenWithRepoTop(t, wtA)
	ctx := context.Background()
	bob := mkFileLock(t, tcAGo, tcBob, time.Hour)
	bob.Worktree = wtB
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.BreakLocks(ctx, []domain.Target{bob.Target}, tcAlice, BreakForce, "force in A", liveProbe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res[0].Err, ErrNoLockAtTarget) {
		t.Errorf("force break in A over a worktree-B row: want ErrNoLockAtTarget, got %v", res[0].Err)
	}
	if owners := ownersAt(t, s, bob.Target); len(owners) != 1 || owners[0] != tcBob {
		t.Errorf("bob's worktree-B lease must survive, holders now %v", owners)
	}
}

// TestBreakLocks_ForceStillBreaksSameAndLegacyWorktree pins the other side:
// a row from this worktree, or a legacy row with no worktree stamp, is still
// in reach of `unlock --force` — scoping may only narrow a foreign row away.
func TestBreakLocks_ForceStillBreaksSameAndLegacyWorktree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		worktree func(wtA string) string
	}{
		{"same worktree", func(wtA string) string { return wtA }},
		{"legacy unset", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wtA := t.TempDir()
			s := mustOpenWithRepoTop(t, wtA)
			ctx := context.Background()
			bob := mkFileLock(t, tcAGo, tcBob, time.Hour)
			bob.Worktree = tc.worktree(wtA)
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe); err != nil {
				t.Fatal(err)
			}
			res, err := s.BreakLocks(ctx, []domain.Target{bob.Target}, tcAlice, BreakForce, "force", liveProbe, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res[0].Err != nil {
				t.Fatalf("break: %v", res[0].Err)
			}
			if owners := ownersAt(t, s, bob.Target); len(owners) != 0 {
				t.Errorf("row must be broken, holders now %v", owners)
			}
		})
	}
}

// TestReleaseLocks_DoesNotReclaimSiblingWorktreeRow: a plain unlock in A over
// a stale row from B reports no-lock and leaves B's row to B.
func TestReleaseLocks_DoesNotReclaimSiblingWorktreeRow(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	s := mustOpenWithRepoTop(t, wtA)
	ctx := context.Background()
	bob := mkFileLock(t, tcAGo, tcBob, time.Hour)
	bob.Worktree = wtB
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReleaseLocks(ctx, []domain.Target{bob.Target}, tcAlice, deadProbe)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].State != StateNoLock {
		t.Errorf("unlock in A over a worktree-B row: want %v, got %v", StateNoLock, res[0].State)
	}
	if owners := ownersAt(t, s, bob.Target); len(owners) != 1 {
		t.Errorf("bob's worktree-B row must survive, holders now %v", owners)
	}
}

// TestAcquireLocks_DoesNotReclaimStaleSiblingWorktreeRow: an acquire in A must
// not reclaim a stale row from B as a side effect — the two never contend, so
// A has no standing to decide B's row is done.
func TestAcquireLocks_DoesNotReclaimStaleSiblingWorktreeRow(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	s := mustOpenWithRepoTop(t, wtA)
	ctx := context.Background()
	bob := mkFileLock(t, tcAGo, tcBob, time.Hour)
	bob.Worktree = wtB
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{bob}, liveProbe); err != nil {
		t.Fatal(err)
	}
	alice := bob
	alice.OwnerUUID, alice.SessionUUID, alice.Worktree = tcAlice, tcAlice, wtA
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{alice}, deadProbe); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, s, bob.Target, EventLockReclaimedStale); n != 0 {
		t.Errorf("acquire in A must not reclaim a worktree-B row, got %d reclaim events", n)
	}
	if owners := ownersAt(t, s, bob.Target); len(owners) != 2 {
		t.Errorf("both worktrees' rows must stand, holders now %v", owners)
	}
}

// loto-8z87: the locks PK was (target_canonical, owner_uuid) only, so ONE
// owner taking the SAME repo-relative path in two linked worktrees (sibling
// sessions sharing one LOTO_AGENT_ID, loto-81n) collided on acquire — the
// second INSERT ... ON CONFLICT silently overwrote the first row's worktree,
// lease and epoch, and the first worktree's lock vanished with no error. The
// worktree is now part of the key: (target_canonical, owner_uuid, worktree).

// TestAcquireLocks_SameOwnerTwoWorktrees_TwoRows: one owner acquiring a.go
// stamped wtA then wtB must end up with two coexisting rows, one per
// worktree — not one row whose worktree flips to the latest acquire.
func TestAcquireLocks_SameOwnerTwoWorktrees_TwoRows(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	s := mustOpenWithRepoTop(t, wtA)
	ctx := context.Background()
	a := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	a.Worktree = wtA
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Worktree = wtB
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LocksAt(ctx, a.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per worktree), got %d: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.OwnerUUID != domain.AgentUUID(tcAlice) {
			t.Errorf("row owner = %s, want %s", r.OwnerUUID, tcAlice)
		}
		seen[r.Worktree] = true
	}
	if !seen[wtA] || !seen[wtB] {
		t.Errorf("want rows stamped %q and %q, got %+v", wtA, wtB, rows)
	}
}

// TestReleaseLocks_SameOwnerTwoWorktrees_ScopedToOwnWorktree: releasing a.go
// from a store opened on wtA must delete only wtA's row — the wtB row, held
// by the SAME owner, must survive. Before loto-8z87 this could not even be
// expressed (the two acquires above collapsed onto one row); it now exercises
// the owner-scoped DELETE, which must not reach past its own worktree either.
func TestReleaseLocks_SameOwnerTwoWorktrees_ScopedToOwnWorktree(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	s := mustOpenWithRepoTop(t, wtA)
	ctx := context.Background()
	a := mkFileLock(t, tcAGo, tcAlice, time.Hour)
	a.Worktree = wtA
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Worktree = wtB
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, tcAlice, liveProbe)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].State != StateUnlocked {
		t.Fatalf("release from wtA must report unlocked, got %+v", res[0])
	}

	rows, err := s.LocksAt(ctx, a.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Worktree != wtB {
		t.Fatalf("wtB's row must survive the wtA release alone, got %+v", rows)
	}
}
