package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// seedLegacyHookBookkeepingDB hand-creates a DB at the pre-loto-v6xx shape:
// hook_calls with no worktree column, path_seq and path_observed keyed
// (path_canonical, epoch) with no worktree column, and tree_events with no
// worktree column — the shape every DB on main carries before this bead.
// Inserts one row per table so TestMigrate_HookBookkeepingWorktreeSurvivesUpgrade
// can assert nothing is lost across the rebuild. Modeled on
// seedLegacyLocksDB's shape and PRAGMA user_version convention
// (migrate_mode_test.go).
func seedLegacyHookBookkeepingDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", connDSN(dbPath))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	const legacyDDL = `
CREATE TABLE locks (
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
  PRIMARY KEY (target_canonical, owner_uuid)
);
CREATE INDEX idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX idx_locks_session  ON locks(session_uuid);
CREATE INDEX idx_locks_expires  ON locks(expires_at);
CREATE TABLE events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded','lock_refreshed','gate_bypass','candidate_accepted','candidate_rejected')),
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
CREATE TABLE hook_calls (
  call_id      TEXT PRIMARY KEY,
  owner_uuid   TEXT NOT NULL,
  session_uuid TEXT NOT NULL DEFAULT '',
  tool_name    TEXT NOT NULL DEFAULT '',
  t_pre        INTEGER NOT NULL,
  t_post       INTEGER,
  post_missing INTEGER NOT NULL DEFAULT 0,
  dead_at      INTEGER
);
CREATE TABLE hook_call_paths (
  call_id        TEXT NOT NULL,
  path_canonical TEXT NOT NULL,
  locked         INTEGER NOT NULL DEFAULT 0,
  declared       INTEGER NOT NULL DEFAULT 0,
  pre_observed   INTEGER NOT NULL DEFAULT 1,
  epoch_pre      INTEGER NOT NULL DEFAULT 0,
  holder_pre     TEXT NOT NULL DEFAULT '',
  seq_pre        INTEGER NOT NULL DEFAULT 0,
  seq_post       INTEGER,
  digest_pre     TEXT NOT NULL DEFAULT '',
  digest_post    TEXT,
  stat_pre       TEXT NOT NULL DEFAULT '',
  stat_post      TEXT,
  PRIMARY KEY (call_id, path_canonical)
);
CREATE TABLE path_seq (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  seq            INTEGER NOT NULL,
  digest         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path_canonical, epoch)
);
CREATE TABLE path_observed (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  stat           TEXT NOT NULL DEFAULT '',
  digest         TEXT NOT NULL DEFAULT '',
  observed_at    INTEGER NOT NULL,
  PRIMARY KEY (path_canonical, epoch)
);
CREATE TABLE tree_events (
  event_id       TEXT PRIMARY KEY,
  path_canonical TEXT NOT NULL,
  epoch_pre      INTEGER NOT NULL DEFAULT 0,
  holder_pre     TEXT NOT NULL DEFAULT '',
  holder_now     TEXT NOT NULL DEFAULT '',
  epoch_now      INTEGER NOT NULL DEFAULT 0,
  observer_uuid  TEXT NOT NULL DEFAULT '',
  call_id        TEXT NOT NULL DEFAULT '',
  seq            INTEGER NOT NULL,
  digest_pre     TEXT NOT NULL DEFAULT '',
  digest_post    TEXT NOT NULL DEFAULT '',
  declared       INTEGER NOT NULL DEFAULT 0,
  rule           TEXT NOT NULL DEFAULT '',
  note           TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
);
CREATE TABLE tree_event_spanners (
  event_id   TEXT NOT NULL,
  call_id    TEXT NOT NULL,
  owner_uuid TEXT NOT NULL,
  PRIMARY KEY (event_id, call_id)
);
CREATE TABLE tree_reports (
  report_id      TEXT PRIMARY KEY,
  event_id       TEXT NOT NULL,
  addressee_uuid TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  delivered_at   INTEGER,
  acted_at       INTEGER
);
PRAGMA user_version = 9;`
	if _, err := db.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatalf("seed legacy ddl: %v", err)
	}

	seedRows := []string{
		`INSERT INTO hook_calls(call_id, owner_uuid, session_uuid, tool_name, t_pre, t_post, post_missing, dead_at)
		 VALUES ('legacy-call', 'owner-legacy', 'sess-legacy', 'Bash', 1, 2, 0, NULL)`,
		`INSERT INTO hook_call_paths(call_id, path_canonical, locked, declared, pre_observed, epoch_pre, holder_pre, seq_pre, seq_post, digest_pre, digest_post, stat_pre, stat_post)
		 VALUES ('legacy-call', 'a.go', 0, 0, 1, 0, '', 0, 1, 'd0', 'd1', 's0', 's1')`,
		`INSERT INTO path_seq(path_canonical, epoch, seq, digest) VALUES ('a.go', 0, 1, 'd1')`,
		`INSERT INTO path_observed(path_canonical, epoch, stat, digest, observed_at) VALUES ('a.go', 0, 's1', 'd1', 3)`,
		`INSERT INTO tree_events(event_id, path_canonical, epoch_pre, holder_pre, holder_now, epoch_now, observer_uuid, call_id, seq, digest_pre, digest_post, declared, rule, note, created_at)
		 VALUES ('legacy-event', 'a.go', 0, '', '', 0, 'owner-legacy', 'legacy-call', 1, 'd0', 'd1', 0, 'row8', 'n', 4)`,
	}
	for _, stmt := range seedRows {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed row %q: %v", stmt, err)
		}
	}
}

// TestMigrate_HookBookkeepingWorktreeSurvivesUpgrade is the bead's third
// acceptance criterion (loto-v6xx): an existing DB — one predating the
// worktree column on hook_calls/tree_events and the worktree-widened PK on
// path_seq/path_observed — migrates in place, with every legacy row surviving
// (worktree = ”, the legacy line) and the new shape in force for new writes.
func TestMigrate_HookBookkeepingWorktreeSurvivesUpgrade(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	seedLegacyHookBookkeepingDB(t, dbPath)

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open legacy db (migrate): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// (a) every legacy row survives, in every one of the four tables.
	for _, table := range []string{"hook_calls", "path_seq", "path_observed", "tree_events"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("%s: legacy row lost in rebuild, got count=%d want 1", table, n)
		}
	}

	// (b) the legacy row reads back with worktree = '' on every table that
	// carries the column, so today's (unscoped) behavior is unchanged for it.
	for _, q := range []string{
		`SELECT worktree FROM hook_calls WHERE call_id = 'legacy-call'`,
		`SELECT worktree FROM path_seq WHERE path_canonical = 'a.go'`,
		`SELECT worktree FROM path_observed WHERE path_canonical = 'a.go'`,
		`SELECT worktree FROM tree_events WHERE event_id = 'legacy-event'`,
	} {
		var wt string
		if err := s.db.QueryRowContext(ctx, q).Scan(&wt); err != nil {
			t.Fatalf("probe worktree (%s): %v", q, err)
		}
		if wt != "" {
			t.Errorf("legacy row worktree: want '', got %q (query %s)", wt, q)
		}
	}

	// (c) path_seq and path_observed's PK is now the 3-column
	// (path_canonical, worktree, epoch), not the legacy 2-column form.
	for _, table := range []string{"path_seq", "path_observed"} {
		var pkCols int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info(?) WHERE pk > 0`, table).Scan(&pkCols); err != nil {
			t.Fatalf("probe %s pk: %v", table, err)
		}
		if pkCols != 3 {
			t.Errorf("%s: want 3-column PK after migrate, got %d", table, pkCols)
		}
	}

	// (d) new writes after the upgrade take the CURRENT (scoped) behavior:
	// the legacy row's own seq keeps advancing under worktree '', a new
	// worktree gets its own independent line.
	seqLegacy, err := s.PathSeq(ctx, "a.go", "", 0)
	if err != nil {
		t.Fatalf("path seq (legacy worktree): %v", err)
	}
	if seqLegacy != 1 {
		t.Fatalf("legacy worktree's seq: want 1 (preserved), got %d", seqLegacy)
	}
	seqNew, err := s.PathSeq(ctx, "a.go", "/repo/wtNew", 0)
	if err != nil {
		t.Fatalf("path seq (new worktree): %v", err)
	}
	if seqNew != 0 {
		t.Fatalf("a fresh worktree's seq: want 0 (independent line), got %d", seqNew)
	}
}
