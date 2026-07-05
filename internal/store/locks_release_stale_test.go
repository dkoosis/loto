package store

// Stale-aware ReleaseLocks matrix (loto-ebkc D1): a plain unlock at a target
// whose foreign holders are ALL stale reclaims them in the same tx — delete +
// lock_reclaimed_stale audit + chmod restore — instead of bouncing to
// not-owner (whose only recourse was --force, the wrong audit kind). One live
// foreign holder vetoes the whole target (authorizeHolders, loto-w77f parity);
// the caller's own row always wins first (prefer-own, loto-k5el.2).

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"loto/internal/domain"
)

// deadProbe (locks_shared_test.go) reports every pid dead; liveProbe
// (locks_shared_test.go) every pid alive.

// tcHost matches mkFileLock's Host so the probe is consulted for same-host rows.
const tcHost = "h"

func countEvents(t *testing.T, s *Store, target domain.Target, kind string) int {
	t.Helper()
	events, err := s.EventsForTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// Matrix row 1: foreign TTL-expired exclusive → reclaimed, audited with the
// dead owner as subject, owner-write restored.
func TestReleaseLocks_ForeignExpiredExclusive_Reclaimed(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(l.Target.Canonical); st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("precondition: acquire should strip write, got %o", st.Mode().Perm())
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, tcHost, deadProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if len(res) != 1 || res[0].State != StateReclaimedStale {
		t.Fatalf("want StateReclaimedStale, got %+v", res)
	}
	if res[0].Owner != tcAlice {
		t.Errorf("Owner = %q, want dead holder %q", res[0].Owner, tcAlice)
	}
	if got, _ := s.LockAt(ctx, l.Target); got != nil {
		t.Fatalf("stale row should be gone, got %+v", got)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("reclaim must restore owner-write, got %o", st.Mode().Perm())
	}
	events, err := s.EventsForTarget(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Kind == EventLockReclaimedStale {
			found = true
			if e.ActorUUID != tcBob {
				t.Errorf("ActorUUID = %q, want %q", e.ActorUUID, tcBob)
			}
			if e.SubjectUUID != tcAlice {
				t.Errorf("SubjectUUID = %q, want dead holder %q", e.SubjectUUID, tcAlice)
			}
		}
	}
	if !found {
		t.Fatalf("expected lock_reclaimed_stale event, got %+v", events)
	}
}

// Matrix row 2 (regression): foreign LIVE exclusive stays not-owner — the row
// survives and the file stays stripped.
func TestReleaseLocks_ForeignLiveExclusive_NotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, tcHost, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateNotOwner || res[0].Owner != tcAlice {
		t.Fatalf("want StateNotOwner owner=%s, got %+v", tcAlice, res)
	}
	if got, _ := s.LockAt(ctx, l.Target); got == nil {
		t.Fatal("live foreign row must survive a plain unlock")
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("file must stay stripped under the surviving lock, got %o", st.Mode().Perm())
	}
	if n := countEvents(t, s, l.Target, EventLockReclaimedStale); n != 0 {
		t.Errorf("no reclaim event expected, got %d", n)
	}
}

// Matrix row 3 (TTL authority pin): TTL-expired with a probe that still says
// the pid is ALIVE → reclaimed anyway. Expiry is unconditional
// (staleness_test.go:132); liveness is the accelerator, not a veto.
func TestReleaseLocks_TTLExpiredPidAlive_Reclaimed(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, tcHost, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateReclaimedStale {
		t.Fatalf("TTL expiry must reclaim even with a live pid, got %+v", res)
	}
	if got, _ := s.LockAt(ctx, l.Target); got != nil {
		t.Fatalf("row should be gone, got %+v", got)
	}
}

// Matrix row 4: all foreign shared holders stale → every row deleted and NO
// chmod restore — shared never stripped, and the file may be deliberately
// read-only (shouldRestoreOwnerWrite).
func TestReleaseLocks_AllSharedStale_DeletedNoRestore(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	b := peerOn(a, tcBob, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Deliberately read-only: a spurious restore would flip it writable.
	if err := os.Chmod(a.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, "carol", tcHost, deadProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateReclaimedStale {
		t.Fatalf("want StateReclaimedStale, got %+v", res)
	}
	for _, owner := range []domain.AgentUUID{tcAlice, tcBob} {
		if got, _ := s.LockForOwnerAt(ctx, a.Target, owner); got != nil {
			t.Errorf("%s's stale shared row should be deleted, got %+v", owner, got)
		}
	}
	st, _ := os.Stat(a.Target.Canonical)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("all-shared reclaim must NOT restore owner-write, got %o", st.Mode().Perm())
	}
	if n := countEvents(t, s, a.Target, EventLockReclaimedStale); n != 2 {
		t.Errorf("want one reclaim event per dead holder (2), got %d", n)
	}
}

// Matrix row 5: one live + one stale shared holder → the live holder vetoes
// the whole target (loto-w77f): nothing is deleted, not even the stale row.
func TestReleaseLocks_MixedLiveStaleShared_NotOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	a.PID = 101
	b := peerOn(a, tcBob, domain.ModeShared)
	b.PID = 102
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatal(err)
	}
	bobAlive := func(_ string, pid int, _ int64) bool { return pid == 102 }

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, "carol", tcHost, bobAlive)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateNotOwner {
		t.Fatalf("live co-holder must veto reclaim, got %+v", res)
	}
	if res[0].Owner != tcBob {
		t.Errorf("Owner = %q, want the vetoing live holder %q", res[0].Owner, tcBob)
	}
	for _, owner := range []domain.AgentUUID{tcAlice, tcBob} {
		if got, _ := s.LockForOwnerAt(ctx, a.Target, owner); got == nil {
			t.Errorf("%s's row must survive a vetoed reclaim", owner)
		}
	}
}

// Matrix row 6: the caller's OWN stale row releases as a plain unlock
// (prefer-own unchanged) — StateUnlocked, not reclaimed-stale.
func TestReleaseLocks_OwnStale_Unlocked(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcAlice, tcHost, deadProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateUnlocked {
		t.Fatalf("own stale row must release as StateUnlocked (prefer-own), got %+v", res)
	}
	if got, _ := s.LockAt(ctx, l.Target); got != nil {
		t.Fatalf("row should be gone, got %+v", got)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("own release must restore owner-write, got %o", st.Mode().Perm())
	}
	if n := countEvents(t, s, l.Target, EventLockReclaimedStale); n != 0 {
		t.Errorf("own release must not emit reclaim events, got %d", n)
	}
	if n := countEvents(t, s, l.Target, EventLockReleased); n != 1 {
		t.Errorf("want 1 lock_released event, got %d", n)
	}
}

// Matrix row 7: own live shared + foreign stale shared → prefer-own releases
// only the caller's row; the foreign stale row SURVIVES (no helpful reclaim
// piggybacked on an own-release).
func TestReleaseLocks_OwnLiveSharedForeignStaleShared_OwnOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", tcAlice, time.Hour)
	a.Mode = domain.ModeShared
	a.PID = 101
	b := peerOn(a, tcBob, domain.ModeShared)
	b.PID = 102
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, liveProbe); err != nil {
		t.Fatal(err)
	}
	aliceAlive := func(_ string, pid int, _ int64) bool { return pid == 101 }

	res, err := s.ReleaseLocks(ctx, []domain.Target{a.Target}, tcAlice, tcHost, aliceAlive)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateUnlocked {
		t.Fatalf("want StateUnlocked (prefer-own), got %+v", res)
	}
	if got, _ := s.LockForOwnerAt(ctx, a.Target, tcAlice); got != nil {
		t.Errorf("alice's row should be deleted, got %+v", got)
	}
	if got, _ := s.LockForOwnerAt(ctx, a.Target, tcBob); got == nil {
		t.Error("bob's stale shared row must SURVIVE alice's own-release")
	}
	if n := countEvents(t, s, a.Target, EventLockReclaimedStale); n != 0 {
		t.Errorf("own-release must not reclaim the co-holder, got %d reclaim events", n)
	}
}

// gcTagsTx parity (loto-qg0r): a reclaim deletes the dead holder's host-lock
// row without acking its tags; the same tx must GC the orphaned tag rows.
func TestReleaseLocks_ReclaimGCsDeadHoldersTags(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", tcAlice, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	got, err := s.LockAt(ctx, l.Target)
	if err != nil || got == nil {
		t.Fatalf("LockAt: %v / %+v", err, got)
	}
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(l.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   got.CreatedAt.UnixNano(),
		TaggerUUID:      tcBob,
		Text:            tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, tcBob, tcHost, deadProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateReclaimedStale {
		t.Fatalf("want StateReclaimedStale, got %+v", res)
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE id = ?`, id).Scan(&one)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dead holder's pending tag should be GC'd with the reclaim, got err=%v", err)
	}
}

// Race (-race, linux CI authoritative): release-reclaim vs AcquireLocks on the
// same TTL-expired target, through the countdown barrier of concurrent_test.go.
// Both paths serialize on the op-flock, so the final state MUST be the
// acquirer's row with the file stripped; the release result is Reclaimed (it
// won and reclaimed the dead row first) or NotOwner (the acquirer's lazy-GC
// got there first and its fresh live row vetoed). The old {no row + writable}
// arm is reachable only via a live-holder-veto violation.
func TestConcurrentReleaseReclaimVsAcquire(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	stale := mkFileLock(t, "race.go", tcAlice, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatal(err)
	}

	fresh := peerOn(stale, "carol", domain.ModeExclusive)
	now := time.Now()
	fresh.CreatedAt = now
	fresh.ExpiresAt = now.Add(time.Hour)

	var ready, start, done sync.WaitGroup
	ready.Add(2)
	start.Add(1)
	done.Add(2)

	var releaseRes []ReleaseResult
	var releaseErr, acquireErr error
	go func() {
		defer done.Done()
		ready.Done()
		start.Wait()
		releaseRes, releaseErr = s.ReleaseLocks(ctx, []domain.Target{stale.Target}, tcBob, tcHost, liveProbe)
	}()
	go func() {
		defer done.Done()
		ready.Done()
		start.Wait()
		_, acquireErr = s.AcquireLocks(ctx, []domain.LockRecord{fresh}, liveProbe)
	}()
	ready.Wait()
	start.Done()
	done.Wait()

	if releaseErr != nil {
		t.Fatalf("ReleaseLocks: %v", releaseErr)
	}
	if acquireErr != nil {
		t.Fatalf("AcquireLocks over an expired row must succeed, got %v", acquireErr)
	}
	if got := releaseRes[0].State; got != StateReclaimedStale && got != StateNotOwner {
		t.Errorf("release result must be Reclaimed or NotOwner, got %v", got)
	}
	final, err := s.LockAt(ctx, stale.Target)
	if err != nil {
		t.Fatal(err)
	}
	if final == nil || final.OwnerUUID != "carol" {
		t.Fatalf("final state must be the acquirer's row, got %+v", final)
	}
	st, _ := os.Stat(stale.Target.Canonical)
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("acquirer's exclusive lock must leave the file stripped, got %o", st.Mode().Perm())
	}
}

// TestReleaseLocks_MixedBatch_AckedTagSurvivesReclaimGC pins the review P2:
// one batch releases the caller's own tagged live lock AND reclaims a stale
// foreign holder. The reclaim's tag GC must be targeted at the reclaimed
// holders' rows only — the blanket gcTagsTx orphan clause would eat the acked
// tag row applyOwnedReleasesTx just wrote in this same tx (the own host-lock
// row is already deleted, so the acked tag looks orphaned to it).
func TestReleaseLocks_MixedBatch_AckedTagSurvivesReclaimGC(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// Alice's own live lock on own.go, tagged by bob (pending).
	own := mkFileLock(t, "own.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{own}, liveProbe); err != nil {
		t.Fatal(err)
	}
	ownRow, err := s.LockAt(ctx, own.Target)
	if err != nil || ownRow == nil {
		t.Fatalf("LockAt(own): %v / %+v", err, ownRow)
	}
	ackedID, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(own.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   ownRow.CreatedAt.UnixNano(),
		TaggerUUID:      tcBob,
		Text:            tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bob's stale foreign lock on dead.go, tagged by carol (pending).
	dead := mkFileLock(t, "dead.go", tcBob, -time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{dead}, liveProbe); err != nil {
		t.Fatal(err)
	}
	deadRow, err := s.LockAt(ctx, dead.Target)
	if err != nil || deadRow == nil {
		t.Fatalf("LockAt(dead): %v / %+v", err, deadRow)
	}
	goneID, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(dead.Target.Canonical),
		LockOwnerUUID:   tcBob,
		LockCreatedAt:   deadRow.CreatedAt.UnixNano(),
		TaggerUUID:      "carol",
		Text:            tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.ReleaseLocks(ctx, []domain.Target{own.Target, dead.Target}, tcAlice, tcHost, liveProbe)
	if err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	if res[0].State != StateUnlocked || res[1].State != StateReclaimedStale {
		t.Fatalf("want [Unlocked, ReclaimedStale], got %+v", res)
	}

	// Alice's acked tag row SURVIVES (audit retention owns its lifetime).
	var acked sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT acked_at FROM tags WHERE id = ?`, ackedID).Scan(&acked); err != nil {
		t.Fatalf("acked tag row must survive the same-tx reclaim GC: %v", err)
	}
	if !acked.Valid {
		t.Fatal("own release should have acked the tag")
	}
	// The dead holder's pending tag is gone.
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM tags WHERE id = ?`, goneID).Scan(&one)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("dead holder's pending tag should be GC'd, got err=%v", err)
	}
}
