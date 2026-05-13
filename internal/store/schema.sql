-- loto v5 schema. Applied on every Open(); all DDL is IF NOT EXISTS so re-apply
-- is a no-op. PRAGMA user_version mismatch triggers MoveCorruptAside (start fresh).

CREATE TABLE IF NOT EXISTS locks (
  target_canonical TEXT PRIMARY KEY,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  branch           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);

CREATE TABLE IF NOT EXISTS events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_target ON events(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_events_kind   ON events(event_kind, created_at);

PRAGMA user_version = 5;
