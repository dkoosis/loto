package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// beaconOf turns a lock fixture into the row `loto beacon` mints for the same
// owner and target: shared, pid-less, marked, short TTL.
func beaconOf(l domain.LockRecord, ttl time.Duration) domain.LockRecord {
	now := time.Now()
	l.Mode = domain.ModeShared
	l.PID = 0
	l.ProcStart = 0
	l.Branch = ""
	l.Beacon = true
	l.Intent = "beacon: agent is writing this file"
	l.CreatedAt = now
	l.ExpiresAt = now.Add(ttl)
	return l
}

// TestBeaconYieldsToOwnExplicitLock is the loto-xl4g regression pin (Codex
// #249). AcquireLocks upserts on (target, owner), and an agent's beacon carries
// its own agent uuid — so the gate minting a beacon for an agent that had
// already run `loto lock` REPLACED that agent's row: exclusive → shared,
// durable pid → 0, branch cleared, 30m → 2m. The agent's declared lease
// silently shrank, and `loto guard` then read the downgraded row as this
// session's own beacon and waived it, moving the tree out from under
// uncommitted work — the 2026-08-14 incident, reintroduced by the fix for it.
func TestBeaconYieldsToOwnExplicitLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	held := mkFileLock(t, "a.go", tcAlice, time.Hour)
	held.Mode = domain.ModeExclusive
	held.Branch = "feat/x"
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held}, liveProbe); err != nil {
		t.Fatalf("acquire explicit lock: %v", err)
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beaconOf(held, 2*time.Minute)}, liveProbe); err != nil {
		t.Fatalf("beacon mint over own lock must succeed, got %v", err)
	}

	got, err := s.LockForOwnerAt(ctx, held.Target, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("row vanished")
	}
	if got.EffectiveMode() != domain.ModeExclusive {
		t.Errorf("mode = %q, want exclusive — the beacon downgraded a declared lease", got.EffectiveMode())
	}
	if got.PID != held.PID {
		t.Errorf("pid = %d, want %d — the beacon dropped the durable liveness handle", got.PID, held.PID)
	}
	if got.Branch != held.Branch {
		t.Errorf("branch = %q, want %q", got.Branch, held.Branch)
	}
	if got.IsBeacon() {
		t.Error("row reads as a beacon; guard would waive it and move the tree")
	}
	if got.ExpiresAt.Before(held.ExpiresAt.Add(-time.Second)) {
		t.Errorf("expires_at = %v, want the original %v — the beacon shortened the lease",
			got.ExpiresAt, held.ExpiresAt)
	}
}

// TestBeaconReplacesOwnLapsedLock is the other half of the yield (Codex #252).
// Yielding to a LAPSED explicit lease protects nothing: collectAllBlockers
// skips same-owner rows, so the expired row is neither reclaimed nor refreshed,
// and every peer reads it as stale and free — the store would keep announcing
// the file is available while the agent is mid-edit, which is the exact
// condition `loto beacon` exists to deny.
func TestBeaconReplacesOwnLapsedLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	lapsed := mkFileLock(t, "a.go", tcAlice, time.Hour)
	lapsed.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{lapsed}, liveProbe); err != nil {
		t.Fatalf("acquire explicit lock: %v", err)
	}
	// Age the row out in place — the wall-clock lapse a running agent would hit,
	// without sleeping through a TTL in a test.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE locks SET expires_at = ? WHERE target_canonical = ?`,
		time.Now().Add(-time.Minute).UnixNano(), lapsed.Target.Canonical); err != nil {
		t.Fatal(err)
	}

	beacon := beaconOf(lapsed, 2*time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beacon}, liveProbe); err != nil {
		t.Fatalf("beacon over own lapsed lock: %v", err)
	}

	got, err := s.LockForOwnerAt(ctx, lapsed.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt: %v / %+v", err, got)
	}
	if !got.IsBeacon() {
		t.Error("a lapsed same-owner lease must not block the beacon; peers still read the dead row as free")
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want a live lease — the beacon did not take", got.ExpiresAt)
	}
}

// TestLegacyBeaconRowsBackfilled pins the migration's one carve-out (Codex
// #252). The release before this one minted beacons as shared / pid-0 rows with
// a fixed intent and no marker. Defaulting those to beacon=0 would promote them
// to apparent explicit leases that guard refuses to move past — and the yield
// rule would then decline to overwrite them for as long as their TTL held.
func TestLegacyBeaconRowsBackfilled(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/loto.db"
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	legacyBeacon := beaconOf(mkFileLock(t, "a.go", tcAlice, time.Hour), time.Hour)
	explicit := mkFileLock(t, "b.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{legacyBeacon, explicit}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Rewind to the pre-column shape: the marker did not exist, so both rows
	// looked the same on disk apart from mode/pid/intent.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE locks DROP COLUMN beacon`); err != nil {
		t.Fatalf("drop beacon column: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-open on pre-beacon DB: %v", err)
	}
	defer s2.Close()

	got, err := s2.LockForOwnerAt(ctx, legacyBeacon.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt(legacy beacon): %v / %+v", err, got)
	}
	if !got.IsBeacon() {
		t.Error("a legacy beacon row must migrate as a beacon, not as an explicit lease")
	}
	got, err = s2.LockForOwnerAt(ctx, explicit.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt(explicit): %v / %+v", err, got)
	}
	if got.IsBeacon() {
		t.Error("the backfill must not sweep in an ordinary lock")
	}
}

// A beacon over a beacon is the refresh path: same owner, same target, in-place
// TTL bump on every gated write. That must keep working — it is how a beacon
// stays alive across an agent's run of edits.
func TestBeaconRefreshesOwnBeacon(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	first := beaconOf(mkFileLock(t, "a.go", tcAlice, time.Hour), time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{first}, liveProbe); err != nil {
		t.Fatalf("first beacon: %v", err)
	}
	second := first
	second.ExpiresAt = first.ExpiresAt.Add(time.Minute)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{second}, liveProbe); err != nil {
		t.Fatalf("beacon refresh: %v", err)
	}

	got, err := s.LockForOwnerAt(ctx, first.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt: %v / %+v", err, got)
	}
	if !got.IsBeacon() {
		t.Error("refreshed row lost its beacon marker")
	}
	if !got.ExpiresAt.After(first.ExpiresAt) {
		t.Errorf("expires_at = %v, want later than %v", got.ExpiresAt, first.ExpiresAt)
	}
}

// The yield is one-directional: an explicit lock still upgrades over the
// agent's own beacon. An agent that beacons a file and then declares it must
// end up holding the declaration.
func TestExplicitLockUpgradesOwnBeacon(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	base := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beaconOf(base, time.Minute)}, liveProbe); err != nil {
		t.Fatalf("beacon: %v", err)
	}
	explicit := base
	explicit.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{explicit}, liveProbe); err != nil {
		t.Fatalf("explicit lock over own beacon: %v", err)
	}

	got, err := s.LockForOwnerAt(ctx, base.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt: %v / %+v", err, got)
	}
	if got.IsBeacon() || got.EffectiveMode() != domain.ModeExclusive || got.PID != base.PID {
		t.Errorf("explicit lock did not upgrade the beacon: %+v", got)
	}
}

// The beacon marker survives a round-trip through sqlite — it is persisted
// state, not a shape the reader re-derives (loto-dm4i). A shared, pid-less row
// that nobody marked must read back as NOT a beacon: that is the shape of a
// `loto lock --shared` placed without LOTO_PID, and reading it as a beacon let
// guard's same-session carve-out waive a real lease.
func TestBeaconFlagRoundTrips(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	plain := mkFileLock(t, "a.go", tcAlice, time.Hour)
	plain.Mode = domain.ModeShared
	plain.PID = 0
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{plain}, liveProbe); err != nil {
		t.Fatalf("acquire pid-less shared lock: %v", err)
	}
	got, err := s.LockForOwnerAt(ctx, plain.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt: %v / %+v", err, got)
	}
	if got.IsBeacon() {
		t.Error("an unmarked shared pid-0 lock must not read as a beacon")
	}

	marked := mkFileLock(t, "b.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{beaconOf(marked, time.Minute)}, liveProbe); err != nil {
		t.Fatalf("acquire beacon: %v", err)
	}
	got, err = s.LockForOwnerAt(ctx, marked.Target, tcAlice)
	if err != nil || got == nil {
		t.Fatalf("LockForOwnerAt: %v / %+v", err, got)
	}
	if !got.IsBeacon() {
		t.Error("a marked beacon must read back as one")
	}
}

// TestLocksBeaconEnsuredOnExistingDB pins the no-version-bump migration
// (mirrors TestClaimsTableEnsuredOnExistingDB): a DB stamped at the current
// user_version but predating the beacon column gets it via ensureLocksBeacon on
// the next Open, and the rows it already held survive as non-beacons.
func TestLocksBeaconEnsuredOnExistingDB(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/loto.db"
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	held := mkFileLock(t, "a.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-beacon DB: drop the column, keep user_version stamped.
	// SQLite 3.35+ supports DROP COLUMN, which is what the driver ships.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE locks DROP COLUMN beacon`); err != nil {
		t.Fatalf("drop beacon column: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-open on pre-beacon DB: %v", err)
	}
	defer s2.Close()

	got, err := s2.LockForOwnerAt(ctx, held.Target, tcAlice)
	if err != nil {
		t.Fatalf("beacon column not ensured on existing DB: %v", err)
	}
	if got == nil {
		t.Fatal("migration lost the pre-existing lock row")
	}
	if got.IsBeacon() {
		t.Error("a row taken before beacons existed must read as a lease the agent asked for")
	}
}
