-- loto v5 schema. Applied on every Open(); all DDL is IF NOT EXISTS so re-apply
-- is a no-op. A STALE user_version on this intact schema re-migrates in place
-- (loto-vmym); only a future version or a foreign schema triggers MoveCorruptAside.

CREATE TABLE IF NOT EXISTS locks (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  -- proc_start: holder process start-time read at acquire (opaque, per-OS).
  -- NULL/0 = unknown (legacy rows, or OS without a reader). Defeats PID reuse
  -- in the liveness probe (loto-kwlp). Added in-place to existing DBs via the
  -- guarded ALTER in migrate(); declared here so fresh DBs match without it.
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  -- mode: 'shared' (multi-reader, write-bit NOT stripped) or 'exclusive'
  -- (sole-writer, write-bit stripped). Legacy rows / NULL read as 'exclusive'
  -- to preserve the pre-mode binary-lock semantics (loto-k5el.2). Added in-place
  -- to existing DBs via the guarded table-rebuild in migrate(); declared here so
  -- fresh DBs match. The composite PK (target_canonical, owner_uuid) lets several
  -- shared holders coexist on one target — meaningless for the old binary lock,
  -- mandatory for shared mode.
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  PRIMARY KEY (target_canonical, owner_uuid)
);
CREATE INDEX IF NOT EXISTS idx_locks_target   ON locks(target_canonical);
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);

CREATE TABLE IF NOT EXISTS events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded')),
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

