package store

import (
	"context"
	"os"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestAcquire_SameOwnerModeUpsert covers loto-h760's surviving half: a
// same-owner re-acquire at a different mode must flip the row in place rather
// than blocking against itself or inserting a second row. The file-mode half of
// that bead retired with chmod enforcement (loto-zssw).
func TestAcquire_SameOwnerModeUpsert(t *testing.T) {
	tests := []struct {
		name        string
		firstMode   string
		secondMode  string
		wantRowMode string // lock row mode after the second acquire
	}{
		{
			name:        "excl_then_shared_flips_row",
			firstMode:   domain.ModeExclusive,
			secondMode:  domain.ModeShared,
			wantRowMode: domain.ModeShared,
		},
		{
			name:        "excl_then_excl_stays_exclusive",
			firstMode:   domain.ModeExclusive,
			secondMode:  domain.ModeExclusive,
			wantRowMode: domain.ModeExclusive,
		},
		{
			name:        "shared_then_shared_stays_shared",
			firstMode:   domain.ModeShared,
			secondMode:  domain.ModeShared,
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

			// loto-zssw: whatever the mode transition, the file itself is never
			// touched. mkFileLock writes 0o644.
			fi, err := os.Stat(second.Target.Canonical)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if fi.Mode().Perm() != 0o644 {
				t.Errorf("acquire must not touch mode bits; perm=%v", fi.Mode().Perm())
			}
		})
	}
}

// TestAcquire_OtherOwnerSharedOverExclusiveBlocks guards the scope of the
// loto-h760 upsert: the in-place mode flip is for the SAME owner only. A
// different owner asking for shared on a target held exclusively is a plain
// conflict, not a downgrade.
func TestAcquire_OtherOwnerSharedOverExclusiveBlocks(t *testing.T) {
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

	// Alice's row must stand, unchanged in mode.
	l, err := s.LockForOwnerAt(ctx, excl.Target, tcAlice)
	if err != nil || l == nil {
		t.Fatalf("alice's lock must survive bob's refused acquire: %v / %v", l, err)
	}
	if l.EffectiveMode() != domain.ModeExclusive {
		t.Errorf("row mode = %s, want exclusive", l.EffectiveMode())
	}
}
