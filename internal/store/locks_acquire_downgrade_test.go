package store

import (
	"context"
	"os"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestAcquire_SameOwnerExclusiveToShared_RestoresWriteBit covers loto-h760: a
// same-owner exclusive→shared re-acquire (e.g. `loto lock foo --shared` on a
// file already held exclusively) must restore the owner-write bit the original
// exclusive acquire stripped. The upsert flips the row to shared in place, but
// stripAll skips shared incoming rows and the same-owner row is never a
// reclaim/break candidate, so before the fix nothing restored the bit and the
// owner's own file stayed read-only forever.
func TestAcquire_SameOwnerExclusiveToShared_RestoresWriteBit(t *testing.T) {
	tests := []struct {
		name        string
		firstMode   string
		secondMode  string
		wantWrite   bool   // owner-write bit set after the second acquire?
		wantRowMode string // lock row mode after the second acquire
	}{
		{
			name:        "excl_then_shared_restores",
			firstMode:   domain.ModeExclusive,
			secondMode:  domain.ModeShared,
			wantWrite:   true,
			wantRowMode: domain.ModeShared,
		},
		{
			name:        "excl_then_excl_stays_stripped",
			firstMode:   domain.ModeExclusive,
			secondMode:  domain.ModeExclusive,
			wantWrite:   false,
			wantRowMode: domain.ModeExclusive,
		},
		{
			name:        "shared_then_shared_stays_writable",
			firstMode:   domain.ModeShared,
			secondMode:  domain.ModeShared,
			wantWrite:   true,
			wantRowMode: domain.ModeShared,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustOpen(t)
			ctx := context.Background()

			first := mkFileLock(t, "a.go", tcAlice, time.Hour)
			first.Mode = tt.firstMode
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{first}, liveProbe); err != nil {
				t.Fatalf("first acquire: %v", err)
			}

			second := mkFileLock(t, "a.go", tcAlice, time.Hour)
			second.Mode = tt.secondMode
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{second}, liveProbe); err != nil {
				t.Fatalf("second acquire: %v", err)
			}

			l, err := s.LockForOwnerAt(ctx, second.Target, tcAlice)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if l == nil || l.EffectiveMode() != tt.wantRowMode {
				t.Fatalf("row mode = %v, want %s", l, tt.wantRowMode)
			}

			fi, err := os.Stat(second.Target.Canonical)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			gotWrite := fi.Mode().Perm()&0o200 != 0
			if gotWrite != tt.wantWrite {
				t.Errorf("owner-write bit = %v, want %v (perm=%v)", gotWrite, tt.wantWrite, fi.Mode().Perm())
			}
		})
	}
}

// TestAcquire_OtherOwnerNotDowngradeRestored guards the scope of the loto-h760
// fix: a different owner re-acquiring shared on a target another owner holds
// exclusively must NOT trigger an owner-write restore — the exclusive holder's
// strip stands. (Cross-owner shared-on-exclusive is a blocker, so this asserts
// the acquire is refused and the bit stays stripped, never spuriously restored.)
func TestAcquire_OtherOwnerNotDowngradeRestored(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	excl := mkFileLock(t, "a.go", tcAlice, time.Hour)
	excl.Mode = domain.ModeExclusive
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{excl}, liveProbe); err != nil {
		t.Fatalf("exclusive acquire: %v", err)
	}

	// Same target path, different owner — mkFileLock uses a fresh TempDir per
	// call, so point Bob's record at Alice's file explicitly.
	other := mkFileLock(t, "b.go", tcBob, time.Hour)
	other.Target = excl.Target
	other.Mode = domain.ModeShared
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{other}, liveProbe); err == nil {
		t.Fatalf("shared acquire over another owner's exclusive lock should block")
	}

	// Alice's exclusive strip must stand — never restored by Bob's attempt.
	if fi, _ := os.Stat(excl.Target.Canonical); fi.Mode().Perm()&0o200 != 0 {
		t.Errorf("exclusive holder's write strip must stand; perm=%v", fi.Mode().Perm())
	}
}
