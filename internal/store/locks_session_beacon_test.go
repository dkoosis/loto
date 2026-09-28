package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"loto/internal/domain"
)

// tcDerivedSibling stands in for deriveUUID(session, agent_id): the owner a
// subagent's stamped beacon carries.
const tcDerivedSibling = "derived-sibling"

// sessionBeaconFixture mints a same-target pair for loto-6sf4: the beacon a
// subagent's Write leaves (owner = its derived id, session = beaconSession)
// and the exclusive lock its Bash `loto lock` asks for (owner = the parent
// identity, session sess-1).
func sessionBeaconFixture(t *testing.T, beaconSession string) (beacon, lock domain.LockRecord) {
	t.Helper()
	lock = mkFileLock(t, "a.go", tcAlice, time.Hour)
	lock.SessionUUID = "sess-1"
	lock.Mode = domain.ModeExclusive
	beacon = lock
	beacon.OwnerUUID = tcDerivedSibling
	beacon.SessionUUID = domain.SessionUUID(beaconSession)
	return beaconOf(beacon, 2*time.Minute), lock
}

// TestAcquireBesideSessionBeacons_GrantsOverOwnSessionBeacon is the
// self-block (loto-6sf4): the parent's lock is taken beside its own
// subagent's beacon, and the beacon stays for the write gate to refuse on.
func TestAcquireBesideSessionBeacons_GrantsOverOwnSessionBeacon(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	beacon, lock := sessionBeaconFixture(t, "sess-1")
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beacon}, liveProbe); err != nil {
		t.Fatalf("mint beacon: %v", err)
	}

	if _, err := s.AcquireLocksBesideSessionBeacons(ctx, []domain.LockRecord{lock}, liveProbe, "sess-1"); err != nil {
		t.Fatalf("lock over own session's beacon refused: %v", err)
	}

	rows, err := s.LocksAt(ctx, lock.Target)
	if err != nil {
		t.Fatal(err)
	}
	var sawLock, sawBeacon bool
	for i := range rows {
		switch {
		case rows[i].OwnerUUID == tcAlice && !rows[i].IsBeacon() && rows[i].EffectiveMode() == domain.ModeExclusive:
			sawLock = true
		case rows[i].OwnerUUID == tcDerivedSibling && rows[i].IsBeacon():
			sawBeacon = true
		}
	}
	if !sawLock {
		t.Errorf("no exclusive lock row for the caller; rows=%+v", rows)
	}
	if !sawBeacon {
		t.Errorf("the sibling's beacon was removed; the write gate has nothing to refuse a second sibling on. rows=%+v", rows)
	}
}

// TestAcquireLocks_StillRefusesSessionBeacon: plain AcquireLocks (the beacon
// and hook-admission callers) keeps refusing a same-session sibling's beacon.
func TestAcquireLocks_StillRefusesSessionBeacon(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	beacon, lock := sessionBeaconFixture(t, "sess-1")
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beacon}, liveProbe); err != nil {
		t.Fatalf("mint beacon: %v", err)
	}
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{lock}, liveProbe)
	if _, ok := errors.AsType[*MultiConflictError](err); !ok {
		t.Fatalf("AcquireLocks over a sibling beacon: err = %v, want *MultiConflictError", err)
	}
}

func TestAcquireBesideSessionBeacons_RefusesOtherSessionBeacon(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	beacon, lock := sessionBeaconFixture(t, "sess-other")
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beacon}, liveProbe); err != nil {
		t.Fatalf("mint beacon: %v", err)
	}
	_, err := s.AcquireLocksBesideSessionBeacons(ctx, []domain.LockRecord{lock}, liveProbe, "sess-1")
	if _, ok := errors.AsType[*MultiConflictError](err); !ok {
		t.Fatalf("lock over another session's beacon: err = %v, want *MultiConflictError", err)
	}
}

func TestAcquireBesideSessionBeacons_RefusesSameSessionRealLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_, lock := sessionBeaconFixture(t, "sess-1")
	sibling := lock
	sibling.OwnerUUID = tcDerivedSibling
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{sibling}, liveProbe); err != nil {
		t.Fatalf("sibling's real lock: %v", err)
	}
	_, err := s.AcquireLocksBesideSessionBeacons(ctx, []domain.LockRecord{lock}, liveProbe, "sess-1")
	if _, ok := errors.AsType[*MultiConflictError](err); !ok {
		t.Fatalf("lock over a same-session REAL lock: err = %v, want *MultiConflictError", err)
	}
}

// An empty session names no one: it must not match a beacon that carries no
// session either.
func TestAcquireBesideSessionBeacons_EmptySessionMatchesNothing(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	beacon, lock := sessionBeaconFixture(t, "")
	lock.SessionUUID = ""
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beacon}, liveProbe); err != nil {
		t.Fatalf("mint beacon: %v", err)
	}
	_, err := s.AcquireLocksBesideSessionBeacons(ctx, []domain.LockRecord{lock}, liveProbe, "")
	if _, ok := errors.AsType[*MultiConflictError](err); !ok {
		t.Fatalf("empty session over a session-less beacon: err = %v, want *MultiConflictError", err)
	}
}
