package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpenCurrentDBPerformsNoWrite asserts the steady-state Open path is
// read-only: opening a DB that is already at schemaUserVersion with an intact
// schema must NOT run the migrate write transaction (no DDL re-exec, no
// user_version PRAGMA re-stamp). Reachable from read commands (cmdCheck,
// cmdStatus) via openRuntime → openOnce → migrate, this redundant write took
// SQLite's WAL writer lock on every read and dirtied the DB (loto-0gsu).
//
// The migrate commit is observed through the commitTxFn seam: on an
// already-current Open it must not fire.
func TestOpenCurrentDBPerformsNoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	// First Open creates + migrates the DB to the current version.
	s, err := OpenContext(context.Background(), path)
	if err != nil {
		t.Fatalf("initial OpenContext: %v", err)
	}
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaUserVersion {
		t.Fatalf("setup: user_version = %d, want %d", v, schemaUserVersion)
	}
	s.Close()

	// Reopen an already-current DB. The migrate write tx must not commit.
	origCommit := commitTxFn
	defer func() { commitTxFn = origCommit }()
	commits := 0
	commitTxFn = func(tx *sql.Tx) error {
		commits++
		return origCommit(tx)
	}

	s2, err := OpenContext(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen OpenContext: %v", err)
	}
	defer s2.Close()

	if commits != 0 {
		t.Fatalf("steady-state Open committed %d write tx, want 0 — migrate must be read-only when already at current version", commits)
	}
}

// TestOpenStaleDBStillMigrates guards the loto-vmym window: a structurally
// intact DB with a stale (below-target) user_version must still run the full
// migrate write tx and re-stamp user_version. The no-write fast path for
// loto-0gsu must NOT swallow this case.
func TestOpenStaleDBStillMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	// Create a current, intact DB.
	s, err := OpenContext(context.Background(), path)
	if err != nil {
		t.Fatalf("initial OpenContext: %v", err)
	}
	s.Close()

	// Simulate the loto-vmym crash window: schema intact but version stale.
	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("stamp stale version: %v", err)
	}
	db.Close()

	// Reopen — must re-migrate in place and re-stamp the current version.
	origCommit := commitTxFn
	defer func() { commitTxFn = origCommit }()
	commits := 0
	commitTxFn = func(tx *sql.Tx) error {
		commits++
		return origCommit(tx)
	}

	s2, err := OpenContext(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen stale OpenContext: %v", err)
	}
	defer s2.Close()

	if commits == 0 {
		t.Fatal("stale DB Open did not run the migrate write tx — loto-vmym re-stamp path must still fire")
	}
	var v int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != schemaUserVersion {
		t.Fatalf("user_version = %d after re-migrate, want %d", v, schemaUserVersion)
	}
}
