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
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_target     ON events(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_events_kind       ON events(event_kind, created_at);
CREATE INDEX IF NOT EXISTS idx_events_created_id ON events(created_at, id);

CREATE TABLE IF NOT EXISTS tags (
  id                TEXT PRIMARY KEY,
  target_canonical  TEXT NOT NULL,
  lock_owner_uuid   TEXT NOT NULL,
  lock_created_at   INTEGER NOT NULL,
  tagger_uuid       TEXT NOT NULL,
  text              TEXT NOT NULL CHECK (length(text) <= 4096),
  created_at        INTEGER NOT NULL,
  acked_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tags_host
  ON tags(target_canonical, lock_owner_uuid, lock_created_at);
CREATE INDEX IF NOT EXISTS idx_tags_holder_pending
  ON tags(lock_owner_uuid, acked_at);

PRAGMA user_version = 9;
