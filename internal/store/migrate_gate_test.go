package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestSchemaCurrentIsDryRunOverEnsures is the loto-t8dd anti-drift guard. The
// steady-state no-write gate (schemaCurrent) must be a dry-run pass over the
// SAME migrationEnsures list migrate's apply path iterates — never a hand-
// mirrored set of probes that can silently fall behind a newly added ensure.
//
// It asserts the two halves of that contract:
//  1. On a fully-current DB, every ensure reports not-pending in dry-run
//     (apply=false) AND schemaCurrent returns true.
//  2. schemaCurrent is true exactly when no ensure is pending — proven by
//     reverting one ensure's upgrade (drop the locks.mode column shape) and
//     observing both that ensure go pending and the gate flip to false.
func TestSchemaCurrentIsDryRunOverEnsures(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	s, err := OpenContext(ctx, path)
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer s.Close()

	// (1) Fully current: no ensure pending, gate true.
	for _, step := range migrationEnsures {
		pending, err := step.fn(ctx, s.db, false)
		if err != nil {
			t.Fatalf("dry-run %s: %v", step.name, err)
		}
		if pending {
			t.Errorf("%s reports pending on a fully-current DB", step.name)
		}
	}
	if !schemaCurrent(ctx, s.db) {
		t.Fatal("schemaCurrent false on a fully-current DB")
	}

	// (2) Make exactly one ensure pending by reverting the locks table to the
	// legacy single-PK / no-mode shape (the pre-loto-k5el.2 layout). The gate
	// must notice without any probe specific to this fact — it falls out of the
	// dry-run pass.
	const revert = `
CREATE TABLE locks_legacy (
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
DROP TABLE locks;
ALTER TABLE locks_legacy RENAME TO locks;`
	if _, err := s.db.ExecContext(ctx, revert); err != nil {
		t.Fatalf("revert locks shape: %v", err)
	}

	if schemaCurrent(ctx, s.db) {
		t.Error("schemaCurrent true after reverting locks to the legacy shape — gate drifted")
	}
	// And the migrate apply path must bring it back (re-stamp not needed; the
	// ensure rebuild restores the composite-PK/mode shape).
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if !schemaCurrent(ctx, s.db) {
		t.Fatal("schemaCurrent false after re-migrate")
	}
}

// TestSchemaCurrentTreatsProbeFailureAsNotCurrent confirms the conservative
// fallback survives the refactor: a structurally-broken DB (no locks table)
// is never reported current, so Open always falls through to migrate.
func TestSchemaCurrentTreatsProbeFailureAsNotCurrent(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", connDSN(filepath.Join(t.TempDir(), "bare.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if schemaCurrent(ctx, db) {
		t.Error("schemaCurrent true on an empty DB with no schema")
	}
}
