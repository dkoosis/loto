package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestLocksWorktreeKeyedOnExistingDB pins the no-version-bump migration for
// loto-8z87 (mirrors TestLocksBeaconEnsuredOnExistingDB): a DB whose locks
// table still carries the OLD (target_canonical, owner_uuid) primary key gets
// rebuilt onto (target_canonical, owner_uuid, worktree) on the next Open, the
// row it already held survives untouched, and the new key actually admits a
// second worktree for the same owner afterward.
func TestLocksWorktreeKeyedOnExistingDB(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/loto.db"
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	held := mkFileLock(t, "a.go", tcAlice, time.Hour)
	held.Worktree = "wtA"
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held}, liveProbe); err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-loto-8z87 DB: rebuild locks with the legacy 2-column PK,
	// keeping every column and row exactly as today's schema holds them.
	const legacyPK = `
CREATE TABLE locks_old (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  beacon           INTEGER NOT NULL DEFAULT 0,
  epoch            INTEGER NOT NULL DEFAULT 0,
  worktree         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (target_canonical, owner_uuid)
);
INSERT INTO locks_old
  (target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at,
   host, pid, proc_start, branch, mode, beacon, epoch, worktree)
SELECT target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at,
       host, pid, proc_start, branch, mode, beacon, epoch, worktree
FROM locks;
DROP TABLE locks;
ALTER TABLE locks_old RENAME TO locks;`
	if _, err := s.db.ExecContext(ctx, legacyPK); err != nil {
		t.Fatalf("simulate legacy PK: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(p)
	if err != nil {
		t.Fatalf("re-open on legacy-PK DB: %v", err)
	}
	defer s2.Close()

	rows, err := s2.LocksAt(ctx, held.Target)
	if err != nil {
		t.Fatalf("LocksAt after migration: %v", err)
	}
	if len(rows) != 1 || rows[0].Worktree != "wtA" || rows[0].OwnerUUID != domain.AgentUUID(tcAlice) {
		t.Fatalf("migration must preserve the pre-existing row untouched, got %+v", rows)
	}

	// The rebuilt key must actually admit a second worktree for the same owner.
	other := held
	other.Worktree = "wtB"
	if _, err := s2.AcquireLocks(ctx, []domain.LockRecord{other}, liveProbe); err != nil {
		t.Fatalf("acquire second worktree after migration: %v", err)
	}
	rows, err = s2.LocksAt(ctx, held.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("post-migration PK must admit both worktrees, got %d rows: %+v", len(rows), rows)
	}
}
