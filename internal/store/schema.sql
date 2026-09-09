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
  -- The IN-list below is a placeholder token, substituted with the quoted,
  -- comma-joined allEventKinds list (event_kinds.go) before this file is
  -- ever executed — see eventKindCheckPlaceholder in schema_embed.go. Never
  -- hand-edit the token; add a new kind in event_kinds.go instead (loto-123y).
  event_kind       TEXT NOT NULL CHECK (event_kind IN (__EVENT_KIND_CHECK__)),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  detail           TEXT NOT NULL DEFAULT '',
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

-- violations: a recorded unauthorized mutation — working-tree content that
-- differs from refs/loto/integration on a path nothing authorized
-- (loto-ovno.9; git-gate.md Phase 5 "sticky violations"). STICKY: the open row
-- survives a lease acquired afterwards, which is what stops a leaseholder
-- laundering a rogue edit it never noticed. No culprit column — the sensor
-- reads content, not writers. Added to existing DBs via ensureViolationsTable
-- in migrate() (no user_version bump); declared here so fresh DBs match.
-- baseline: the refs/loto/integration commit the observation was a delta FROM.
-- An acknowledgement ("legitimate and staying") is only meaningful against the
-- baseline it was given — once integration moves, the same path+fingerprint
-- can mean the opposite. Without it, acking a DELETION (whose fingerprint is
-- empty) would suppress every future deletion of that path forever.
CREATE TABLE IF NOT EXISTS violations (
  id             TEXT PRIMARY KEY,
  path_canonical TEXT NOT NULL,
  observed_at    INTEGER NOT NULL,
  fingerprint    TEXT NOT NULL DEFAULT '',
  baseline       TEXT NOT NULL DEFAULT '',
  lease_state    TEXT NOT NULL DEFAULT '',
  expected_owner TEXT NOT NULL DEFAULT '',
  resolved_at    INTEGER,
  resolution     TEXT NOT NULL DEFAULT '',
  -- Which checkout the observation was taken in, '' for the primary one.
  -- Worktrees of a repo share this DB, so a row without it cannot say whose
  -- tree is dirty (loto-nper).
  worktree       TEXT NOT NULL DEFAULT ''
);
-- ‡ NO unique index on the open set is declared here, and that is deliberate.
-- The real one is keyed (path_canonical, worktree) — path alone lets the
-- second checkout's open row for a shared path be eaten by RecordViolations'
-- INSERT OR IGNORE (loto-nper) — but migrate runs this whole file BEFORE any
-- ensure step, so on a DB whose violations table predates the worktree column
-- a statement naming that column fails with "no such column" and every
-- command that opens the store dies. ensureViolationsOpenIndexScoped creates
-- it instead, after ensureViolationsWorktree has added the column, and drops
-- the unscoped form wherever it is still standing (Codex #283 P1).
CREATE INDEX IF NOT EXISTS idx_violations_open ON violations(resolved_at, path_canonical);

-- hook_calls / hook_call_paths / path_seq: the tree hooks' call-record layer
-- (enforcement-design.md §3 "call" and "seq", §5 I3 step 4, I4 step 5). One
-- row per harness tool call, one row per path that call observed, and the
-- per-path transition sequence keyed (f, E) that both cite. Added to existing
-- DBs via ensureHookCallsTables in migrate() (no user_version bump); declared
-- here so fresh DBs match. Shapes and rationale: hook_calls.go.
CREATE TABLE IF NOT EXISTS hook_calls (
  call_id      TEXT PRIMARY KEY,
  owner_uuid   TEXT NOT NULL,
  session_uuid TEXT NOT NULL DEFAULT '',
  tool_name    TEXT NOT NULL DEFAULT '',
  -- In flight = t_post NULL AND dead_at NULL. post_missing is INDEPENDENT of
  -- both: a call past T_report is flagged and stays in flight (§3, round 10).
  t_pre        INTEGER NOT NULL,
  t_post       INTEGER,
  post_missing INTEGER NOT NULL DEFAULT 0,
  -- dead_at: when the liveness probe found this call's owner dead — §3's
  -- second ending ("until either post event lands OR the probe finds s dead").
  -- Kept apart from t_post because the call never posted; retention reads
  -- COALESCE(t_post, dead_at), so a crashed session's record cannot pin these
  -- two tables against the drop forever.
  dead_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_hook_calls_inflight ON hook_calls(t_post, dead_at, t_pre);

CREATE TABLE IF NOT EXISTS hook_call_paths (
  call_id        TEXT NOT NULL,
  path_canonical TEXT NOT NULL,
  locked         INTEGER NOT NULL DEFAULT 0,
  declared       INTEGER NOT NULL DEFAULT 0,
  -- pre_observed 0 = the path entered the observation set at post (it became
  -- dirty, or left the status set, inside the call); its digest_pre is empty
  -- because no hook saw it before the tool ran.
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

CREATE TABLE IF NOT EXISTS path_seq (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  seq            INTEGER NOT NULL,
  -- digest: what the path was when this number was assigned. It answers ONE
  -- question — "is the state I am looking at already numbered?" — so two hooks
  -- observing the SAME physical change file two events against one transition
  -- instead of inventing a second. It is never used to decide contention:
  -- that is the seq interval and nothing else (§3, round 10).
  digest         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (path_canonical, epoch)
);

-- path_observed / tree_events / tree_event_spanners / tree_reports: the drift
-- layer over the call records (enforcement-design.md §3 "observed", "event",
-- "report", §5 I3 step 2 and I4 steps 5-6's report half). Added to existing
-- DBs via ensureTreeEventsTables in migrate() (no user_version bump); declared
-- here so fresh DBs match. Shapes and rationale: tree_events.go.
CREATE TABLE IF NOT EXISTS path_observed (
  path_canonical TEXT NOT NULL,
  epoch          INTEGER NOT NULL,
  stat           TEXT NOT NULL DEFAULT '',
  digest         TEXT NOT NULL DEFAULT '',
  observed_at    INTEGER NOT NULL,
  PRIMARY KEY (path_canonical, epoch)
);

CREATE TABLE IF NOT EXISTS tree_events (
  event_id       TEXT PRIMARY KEY,
  path_canonical TEXT NOT NULL,
  -- epoch_pre / holder_pre are `E_pre` and `h_pre`: the lock generation and
  -- owner as the observing call recorded them. epoch_now / holder_now are the
  -- same two now — a difference is verdict-table row 1.
  epoch_pre      INTEGER NOT NULL DEFAULT 0,
  holder_pre     TEXT NOT NULL DEFAULT '',
  holder_now     TEXT NOT NULL DEFAULT '',
  epoch_now      INTEGER NOT NULL DEFAULT 0,
  -- observer_uuid / call_id are empty for a drift event: I3 step 2 files it
  -- from a pre-hook, and no call's window covered the change.
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
CREATE INDEX IF NOT EXISTS idx_tree_events_path ON tree_events(path_canonical, seq);

CREATE TABLE IF NOT EXISTS tree_event_spanners (
  event_id   TEXT NOT NULL,
  call_id    TEXT NOT NULL,
  owner_uuid TEXT NOT NULL,
  PRIMARY KEY (event_id, call_id)
);

CREATE TABLE IF NOT EXISTS tree_reports (
  report_id      TEXT PRIMARY KEY,
  event_id       TEXT NOT NULL,
  addressee_uuid TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  -- delivered_at NULL is what `loto status` lists and what the addressee's
  -- next pre-hook hands over. acted_at stamps the ONE time §10b row 2 counts
  -- this report as acted on, so a session that keeps working while the file
  -- stays reverted cannot add an acted row per tool call. The acted window
  -- runs from delivered_at, never created_at: a report nobody read moved
  -- nobody.
  delivered_at   INTEGER,
  acted_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tree_reports_undelivered ON tree_reports(delivered_at, addressee_uuid);
