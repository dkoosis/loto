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
  -- beacon: 1 = minted by the PreToolUse gate on a writing agent's behalf,
  -- 0 = a lease an agent asked for. The row shape cannot carry this: a beacon
  -- is shared with pid 0, and so is an ordinary `loto lock --shared` placed
  -- without LOTO_PID, so the old shape test read a real shared lease as a
  -- beacon and let guard's same-session carve-out waive it (loto-dm4i,
  -- Codex #249). Added in-place to existing DBs via the guarded ALTER in
  -- migrate(); declared here so fresh DBs match without it.
  beacon           INTEGER NOT NULL DEFAULT 0,
  -- epoch: generation counter of the AUTHORIZATION to write this path, not of
  -- this row (loto-ovno.2). A renewal (same live owner re-acquiring) leaves it
  -- untouched; a fresh grant after release/reclaim/force-break bumps it via
  -- path_epochs below. Legacy rows default to 0 — indistinguishable from a
  -- first-ever grant, which is the correct reading (nothing captured an
  -- envelope against them). Added in-place via the guarded ALTER in migrate();
  -- declared here so fresh DBs match without it.
  epoch            INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (target_canonical, owner_uuid)
);
-- No standalone target_canonical index: the composite PK's automatic index has
-- target_canonical as its leftmost column, so target-only lookups (the conflict
-- probe's hot path) already use it.
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);

CREATE TABLE IF NOT EXISTS events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded','lock_refreshed','gate_bypass')),
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

-- claims: coarse path-prefix territory reservations ("this package is mine
-- this session"), distinct from per-file locks (loto-7af9). TTL-only leases —
-- no pid/proc_start/mode. The PK admits cross-owner duplicates by design; the
-- in-tx overlap predicate in ClaimPrefix is the real guard. Added in-place to
-- existing DBs via ensureClaimsTable in migrate() (no user_version bump);
-- declared here so fresh DBs match.
CREATE TABLE IF NOT EXISTS claims (
  path_prefix  TEXT NOT NULL,
  owner_uuid   TEXT NOT NULL,
  session_uuid TEXT NOT NULL DEFAULT '',
  intent       TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  host         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path_prefix, owner_uuid)
);
CREATE INDEX IF NOT EXISTS idx_claims_expires ON claims(expires_at);


-- territory_tags: a note pinned to repo territory — an unlocked file path or a
-- directory prefix — for the agent who arrives there next (loto-z3y1). Added to
-- existing DBs via ensureTerritoryTagsTable in migrate() (no user_version
-- bump); declared here so fresh DBs match.
--
-- Deliberately NOT the `tags` table. A tag's lifetime is parasitic on a host
-- lock, enforced in six queries including a hard DELETE ... WHERE NOT EXISTS
-- (lock) — which would silently eat every hostless row on the first
-- doctor --repair. These two lifetimes are structurally unable to collide here.
CREATE TABLE IF NOT EXISTS territory_tags (
  id            TEXT PRIMARY KEY,
  path_prefix   TEXT NOT NULL,
  tagger_uuid   TEXT NOT NULL,
  text          TEXT NOT NULL CHECK (length(text) <= 4096),
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  acked_at      INTEGER,
  acked_by_uuid TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_territory_tags_live ON territory_tags(expires_at, acked_at);

-- path_epochs: the durable, per-path generation counter locks.epoch snapshots
-- at grant time (loto-ovno.2). Survives lock release/reclaim/break — the whole
-- point is a value that outlives the row it eventually seeds, so a LATER
-- acquire at the same path can keep counting up rather than starting over.
-- One row per path ever locked; never deleted (bounded by the repo's distinct
-- file count, not by lock churn). Added to existing DBs via
-- ensurePathEpochsTable in migrate() (no user_version bump); declared here so
-- fresh DBs match.
CREATE TABLE IF NOT EXISTS path_epochs (
  path_canonical TEXT PRIMARY KEY,
  epoch          INTEGER NOT NULL
);

-- candidate_claims: a durable, per-path territory hold on behalf of a
-- candidate awaiting promotion (loto-ovno.2; git-gate.md "Claim lifecycle").
-- One row per (path, candidate) — a candidate's write-set claims every path it
-- touches, mirroring locks' per-file granularity, never claims' prefix
-- granularity. No TTL: liveness is PID+proc_start of the process that minted
-- it (domain.EvalContext.CandidateClaimIsDead) — a candidate under review has
-- no natural deadline, so unlike locks or claims this record's only staleness
-- authority is "was the minting process provably killed." Added to existing
-- DBs via ensureCandidateClaimsTable in migrate() (no user_version bump);
-- declared here so fresh DBs match.
CREATE TABLE IF NOT EXISTS candidate_claims (
  path_canonical TEXT NOT NULL,
  candidate_id   TEXT NOT NULL,
  owner_uuid     TEXT NOT NULL,
  session_uuid   TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  host           TEXT NOT NULL DEFAULT '',
  pid            INTEGER NOT NULL DEFAULT 0,
  proc_start     INTEGER,
  PRIMARY KEY (path_canonical, candidate_id)
);
CREATE INDEX IF NOT EXISTS idx_candidate_claims_candidate ON candidate_claims(candidate_id);
