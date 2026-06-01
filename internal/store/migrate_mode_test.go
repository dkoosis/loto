package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrate_AddsModeColumn asserts a fresh DB carries the locks.mode column
// (loto-k5el.2 T1). Probed via pragma_table_info — no domain dependency, this is
// a migration-layer test.
func TestMigrate_AddsModeColumn(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'mode'`).Scan(&n); err != nil {
		t.Fatalf("probe mode column: %v", err)
	}
	if n != 1 {
		t.Fatalf("want mode column present, got count=%d", n)
	}
}

// TestMigrate_LocksPKIsComposite asserts the locks PK spans two columns
// (target_canonical, owner_uuid) on a fresh DB (loto-k5el.2 T1).
func TestMigrate_LocksPKIsComposite(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	// pragma_table_info.pk is the 1-based position in the PK, 0 if not part of it.
	var pkCols int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		t.Fatalf("probe pk: %v", err)
	}
	if pkCols != 2 {
		t.Fatalf("want composite PK over 2 columns, got %d", pkCols)
	}
}

// TestMigrate_AllowsDowngradeEvent asserts the events CHECK constraint admits
// the new lock_downgraded kind (loto-k5el.2 T6). On a fresh DB this is the
// widened CHECK from schema.sql; the legacy-DB path is exercised by
// TestMigrate_LegacyDBRoundTrip's events probe.
func TestMigrate_AllowsDowngradeEvent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events(id,target_canonical,event_kind,actor_uuid,reason,created_at)
		 VALUES ('e-test','/a.go','lock_downgraded','alice','x',0)`)
	if err != nil {
		t.Fatalf("lock_downgraded must be an allowed event_kind: %v", err)
	}
}

// seedLegacyLocksDB hand-creates a DB at the pre-loto-k5el.2 shape: locks with a
// single-column PRIMARY KEY and no mode column (but WITH proc_start — the v9
// production shape), events with the pre-downgrade CHECK, plus tags. It stamps
// user_version=9 and inserts n live lock rows. Returns nothing; the file at
// dbPath is left closed, ready for store.Open to migrate.
func seedLegacyLocksDB(t *testing.T, dbPath string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", connDSN(dbPath))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	const legacyDDL = `
CREATE TABLE locks (
  target_canonical TEXT PRIMARY KEY,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX idx_locks_session  ON locks(session_uuid);
CREATE INDEX idx_locks_expires  ON locks(expires_at);
CREATE TABLE events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
CREATE TABLE tags (
  id                TEXT PRIMARY KEY,
  target_canonical  TEXT NOT NULL,
  lock_owner_uuid   TEXT NOT NULL,
  lock_created_at   INTEGER NOT NULL,
  tagger_uuid       TEXT NOT NULL,
  text              TEXT NOT NULL CHECK (length(text) <= 4096),
  created_at        INTEGER NOT NULL,
  acked_at          INTEGER
);
PRAGMA user_version = 9;`
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatalf("seed legacy ddl: %v", err)
	}
	for i := range n {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO locks(target_canonical,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,proc_start,branch)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			"/tmp/legacy/"+makeUUID(i)+".go",
			makeUUID(i), makeUUID(i), "legacy", 1, 1<<62, "h", 1, nil, ""); err != nil {
			t.Fatalf("seed legacy lock row %d: %v", i, err)
		}
	}
}

// TestMigrate_LegacyDBRoundTrip drives the actual table-rebuild branch: it opens
// a DB carrying the OLD single-column PK with live rows, migrates it through
// store.Open, and asserts rows survive with mode='exclusive', the PK is now
// composite, lock_downgraded events are accepted, and the DB was migrated in
// place (no MoveCorruptAside sibling). Closes Open Q4 (loto-k5el.2, PR A).
func TestMigrate_LegacyDBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	seedLegacyLocksDB(t, dbPath, 2)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open legacy db (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// (a) both legacy rows survived the rebuild.
	var rowCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM locks`).Scan(&rowCount); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("legacy rows lost in rebuild: got %d want 2", rowCount)
	}

	// (b) every migrated row defaults to mode='exclusive'.
	var nonExclusive int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM locks WHERE mode IS NOT 'exclusive'`).Scan(&nonExclusive); err != nil {
		t.Fatalf("probe migrated mode: %v", err)
	}
	if nonExclusive != 0 {
		t.Fatalf("legacy rows must default to exclusive, got %d non-exclusive", nonExclusive)
	}

	// (c) PK is now composite (2 columns).
	var pkCols int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		t.Fatalf("probe migrated pk: %v", err)
	}
	if pkCols != 2 {
		t.Fatalf("want composite PK after migrate, got %d", pkCols)
	}

	// (d) events CHECK now admits lock_downgraded on the migrated (rebuilt) DB.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO events(id,target_canonical,event_kind,actor_uuid,reason,created_at)
		 VALUES ('e-legacy','/a.go','lock_downgraded','alice','x',0)`); err != nil {
		t.Fatalf("migrated events table must admit lock_downgraded: %v", err)
	}

	// (e) migrated in place — no .corrupt move-aside sibling beside the DB.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		// moveCorruptAside (doctor.go) creates a sibling named
		// "<dbPath>.corrupt.<stamp>/" (or ".corrupt-staging-"/".corrupt.failed.").
		if name := e.Name(); strings.Contains(name, ".corrupt") {
			t.Fatalf("migrate moved the DB aside (found %q) — legacy rows would be lost", name)
		}
	}
}
