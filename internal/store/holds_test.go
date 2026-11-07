package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

func mkHold(target string, dir bool, owner domain.AgentUUID) domain.LockRecord {
	now := time.Now()
	return domain.LockRecord{
		Target: domain.Target{Canonical: target}, Directory: dir, MayCreate: !dir,
		OwnerUUID: owner, SessionUUID: domain.SessionUUID(owner), Intent: tcTest,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: tcHost, PID: 1,
	}
}

// A directory hold covers every path beneath it, so a peer's file hold there
// is refused, and a peer's file hold refuses a directory hold above it.
func TestDirectoryHold_AndFileHoldBeneathRefuseEachOther(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, first, second string }{
		{"directory first", tcPkgA, "pkg/a/x.go"},
		{"file first", "pkg/a/x.go", tcPkgA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustOpen(t)
			first := mkHold(tc.first, tc.first == tcPkgA, tcAlice)
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{first}, liveProbe); err != nil {
				t.Fatalf("first hold: %v", err)
			}
			second := mkHold(tc.second, tc.second == tcPkgA, tcBob)
			if _, err := s.AcquireLocks(ctx, []domain.LockRecord{second}, liveProbe); !isConflictErr(err) {
				t.Fatalf("peer's %s hold under alice's %s: err = %v, want a conflict", tc.second, tc.first, err)
			}
		})
	}
}

// Opening a store that still has a claims table folds each live claim into a
// directory hold, lets each expired one lapse, and drops the table.
func TestOpen_FoldsClaimsIntoHolds(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "loto.db")
	s, err := OpenContext(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE claims (
  path_prefix TEXT NOT NULL, owner_uuid TEXT NOT NULL, session_uuid TEXT NOT NULL DEFAULT '',
  intent TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  host TEXT NOT NULL DEFAULT '', worktree TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path_prefix, owner_uuid))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO claims VALUES
  ('pkg/live', ?, ?, 'refactor', ?, ?, 'h', ''),
  ('pkg/dead', ?, ?, 'old', ?, ?, 'h', '')`,
		tcAlice, tcAlice, now.UnixNano(), now.Add(time.Hour).UnixNano(),
		tcBob, tcBob, now.Add(-2*time.Hour).UnixNano(), now.Add(-time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = reopen(t, dbPath, true)
	holds, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Target.Canonical != "pkg/live" || holds[0].OwnerUUID != tcAlice || holds[0].Intent != "refactor" {
		t.Fatalf("want alice's live claim as the one hold, got %+v", holds)
	}
	if tableExists(ctx, s.db, "claims") {
		t.Error("the claims table survives the fold")
	}
}
