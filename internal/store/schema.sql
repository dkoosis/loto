-- loto v4 schema. Applied on every Open(); all DDL is IF NOT EXISTS so re-apply is a no-op.
-- No version tracking, no migrator. If schema needs to change, replace this file
-- and start each project DB fresh.

CREATE TABLE IF NOT EXISTS locks (
  target_canonical TEXT PRIMARY KEY,
  target_kind      TEXT NOT NULL CHECK (target_kind IN ('file','dir','glob')),
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

CREATE TABLE IF NOT EXISTS tags (
  target_canonical    TEXT NOT NULL,
  id                  TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN ('note','system')),
  event               TEXT,
  target_kind         TEXT NOT NULL CHECK (target_kind IN ('file','dir','glob')),
  author_uuid         TEXT NOT NULL,
  addressee_uuid      TEXT,
  previous_owner_uuid TEXT,
  intent              TEXT NOT NULL,
  created_at          INTEGER NOT NULL,
  expires_at          INTEGER,
  PRIMARY KEY (target_canonical, id)
);
CREATE INDEX IF NOT EXISTS idx_tags_target     ON tags(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_tags_addressee  ON tags(addressee_uuid, created_at);
CREATE INDEX IF NOT EXISTS idx_tags_expires    ON tags(expires_at);

CREATE TABLE IF NOT EXISTS read_cursors (
  agent_uuid       TEXT NOT NULL,
  target_canonical TEXT NOT NULL,
  last_read_at     INTEGER NOT NULL,
  PRIMARY KEY (agent_uuid, target_canonical)
);
CREATE INDEX IF NOT EXISTS idx_cursors_agent ON read_cursors(agent_uuid);

CREATE TABLE IF NOT EXISTS schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id         TEXT PRIMARY KEY,
  from_uuid  TEXT NOT NULL,
  to_uuid    TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  read_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages(to_uuid, read_at);

PRAGMA user_version = 4;
