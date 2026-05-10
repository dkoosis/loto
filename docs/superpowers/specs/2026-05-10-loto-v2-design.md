# loto v2 design

*Author: Claude (FullHorse session). Audience: dk + future Claudes. Supersedes the contested portions of `docs/NORTH_STAR.md`; consistent portions still apply.*

## Goal

Support multi-agent work in one repo with two primitives matching the industrial Lockout/Tagout (LOTO) procedure:

1. **Lock-out** a target so other agents' writes fail.
2. **Tag-out** a target with informational notes, including notes addressed to a specific agent.

No daemon. No network. Single host. Reads are free; loto coordinates writes only.

## Mental model: LOTO

The OSHA Lockout/Tagout procedure governs servicing of equipment where unexpected energization could harm someone. The shape of the procedure — not its detail — is the model:

- **Lockout** is a physical padlock on a switch. One person, one lock, one key. Nobody else can remove it.
- **Tagout** is a written warning attached to a switch. Anyone can read it. Multiple tags coexist. Tags inform; they don't enforce.
- A worker may attach a tag to their own locked switch ("doing X, done by 14:00") or attach a tag to someone else's locked switch ("ping me when you're free").

Borrow the shape. Don't translate every detail. Specifically: **no multi-lock hasp** — a target has at most one lock-holder.

## Two primitives

### Lock

Exclusive claim by one agent on one target. Target forms, in order of preference:

1. **Exact file path** — `internal/store/store.go`. Overlap is identity comparison. Trivial.
2. **Directory prefix** — `internal/store/` (trailing slash distinguishes from a file of the same name). Locks every path under that subtree. Overlap is path-prefix comparison. Trivial.
3. **Doublestar glob** — `internal/store/**/*.go`. Locks paths matching the pattern. Overlap requires `patternsOverlap` and is gated on the verification commitment in the Concurrency contract. **Use only when forms 1 and 2 are insufficient** — the cases where you actually need to filter by extension or pattern within a subtree, not just claim the subtree.

Forms 1 and 2 cover almost every real use case ("this file" or "this subtree"). Form 3 stays available because some real refactors are extension-scoped, but the agent pays a verification tax — overlap correctness with globs is the most expensive invariant we ship.

- **Single owner.** Only the placing agent releases.
- **Canonical target.** Every operation canonicalizes the target before lookup or write (see Target canonicalization). Equivalent spellings (`./a`, `a`, `a/./`) refer to the same lock; mismatched spellings cannot fragment a lock's identity.
- **Overlap blocks.** A `lock` attempt fails (exit 1) if its canonical target overlaps any existing lock owned by a different agent. Overlap is symmetric: any path that could match both targets counts. Same-agent re-lock is idempotent (refreshes intent/TTL in place).
- **TTL.** Every lock carries an expiry. Default `--ttl` is `30m` when not specified — long enough for a typical edit session, short enough that crashed agents don't strand locks for hours. The owner re-issues `lock` against the same canonical target to extend (idempotent refresh). Past TTL the lock is *stale*: it still occupies the slot until reclaimed.
- **Stale reclaim.** Stale locks (past TTL or with a dead pid on this host) are eligible for reclaim. When acquiring a target, `lock` reclaims **every stale lock that overlaps the requested target** before evaluating remaining live conflicts — same target or broader subtree alike. Each reclaim writes its own system tag. (Without this, a stale subtree lock would block a same-agent retry forever even after the holder crashes.) `break` reclaims without acquiring. `doctor --repair` reclaims at scale. Reads — `status`, `inbox`, `check-paths` — never reclaim.
- **Recovery break.** `break <target> --reason "..."` reclaims a *stale* lock and writes a system-authored tag explaining the reclaim. Fails (exit 1) if the lock is live. This is the normal recovery path.
- **Forced live takeover.** `break <target> --force --reason "..."` reclaims a *live* lock owned by another agent. The binary writes a system-authored tag on the same target — `kind: "system"`, `author_uuid = breaker`, `previous_owner_uuid = displaced`, `addressee_uuid = displaced` — naming the breaker and the reason. The break-takeover transaction inserts the system tag and replaces the lock atomically; observers never see one without the other. No silent dispossession, ever. No forged authorship, ever.
- **Branch metadata is display-only.** The lock record carries the breaker's branch for orientation; it never affects overlap, takeover, or any other safety decision. Worktrees on different branches still share the same project state and the same overlap rules.
- **Persistence.** Stored as a row in the project SQLite database. All lock-set mutations occur inside a `BEGIN IMMEDIATE` transaction (see Concurrency contract); there is no application-level project mutex.

The blocked-attempt response carries a structured holder report. **Multiple blockers are surfaced together** — when a `lock` request overlaps several live locks (different agents, or one agent's stacked locks), the binary returns exit 1 with every overlapping blocker reported in deterministic order (by `held_since` ascending, then `target_canonical`). Reporting only the first blocker would force the agent into a one-conflict-at-a-time game of whack-a-mole.

```json
{
  "blockers": [
    {
      "blocked_by": "GreenCastle",
      "intent": "store refactor — beads loto-7wp.4",
      "target": "internal/store/store.go",
      "held_since": "2026-05-10T07:14:11Z",
      "expires_at": "2026-05-10T07:24:11Z",
      "host": "dk-mac",
      "pid": 84231
    }
  ]
}
```

### Tag

A note attached to a target. Multiple tags may attach to the same target. Each tag carries:

- `kind` — `"note"` (default, human-authored) or `"system"` (binary-authored, e.g. break-takeover audit)
- author_uuid (agent uuid; handle resolved at display time)
- target (canonical file/dir/glob — same shape as lock targets)
- intent (free-text note)
- optional addressee_uuid (another agent uuid)
- optional TTL (expired tags are eligible for prune at next compaction)
- additional fields for `kind: "system"` events: `event` (e.g. `lock_broken`), `previous_owner_uuid`

Tags are informational only. They neither block nor grant access. Authorship is always truthful — the binary never forges authorship in another agent's name; system events are clearly attributed to the binary on behalf of the triggering agent.

Use cases:

| Use case | Shape |
|---|---|
| "I'm working on X, done by 14:00" | `kind: note`, target=file/glob, no addressee, TTL ≈ work duration |
| "Ping me when you're free with this file" | `kind: note`, target=file, addressee_uuid = lock-holder |
| "I broke the test on line 40 before releasing" | `kind: note`, target=file, no addressee, short TTL |
| Direct message to another agent | `kind: note`, target=any, addressee_uuid = recipient |
| Territory declaration ("I'm sweeping `internal/store/**`") | `kind: note`, target=glob, no addressee |
| `break --force` audit trail | `kind: system`, event=`lock_broken`, author_uuid=breaker, addressee_uuid=displaced, previous_owner_uuid=displaced |
| Stale-lock reclaim audit | `kind: system`, event=`lock_reclaimed_stale`, author_uuid=reclaimer, addressee_uuid=previous owner |

There is no separate "reservation" concept. What the v1 design called a reservation is a tag on a glob.

## Identity

A per-session adjective+noun handle (`FullHorse`, `BlueOak`, `GreenCastle`), assigned at SessionStart, exported as `LOTO_AGENT_ID`. Every shell-out from that session inherits the env, so locks and tags placed by `bash -c "loto ..."` and by direct CLI invocations from the same session are owned by the same identity.

Identity records live at `~/.loto/agents/<uuid>.json` (host-global, session-persistent). State is project-scoped under `$XDG_STATE_HOME/loto/projects/<slug>/`. Same canonical project base across all worktrees of the same repo, so siblings see each other.

### Identity lifecycle

The handle is the *display* form; the **uuid is the canonical identifier.** All on-disk references to an agent — lock owners, tag authors, tag addressees — store the uuid. Handles are resolved to the current display form at read time via the `~/.loto/agents/` registry.

| Concern | Resolution |
|---|---|
| Lock/tag owner field | uuid (canonical). Display layer maps uuid → current handle. |
| Tag addressee (`--to <agent>`) | The CLI accepts a handle, looks it up at *write* time, and stores the uuid. If the handle is unknown locally, the write fails (exit 2) with a "no such agent on this host" error rather than silently storing an unresolvable string. |
| Handle reuse across sessions | Handles are not guaranteed unique across time: a future session may be assigned the same noun+adjective. Resolution is by uuid only; the handle is decoration. |
| Cross-host addressing | Out of scope — we don't coordinate across hosts (see non-goals). The agent registry is host-local. |
| Bootstrap from non-Claude shells | A bare invocation (`bash -c "loto lock ..."` from cron, scheduled tasks, manual shell) without `LOTO_AGENT_ID` set creates a new ad-hoc identity on first use. The new uuid is registered with handle = `Adhoc-<short-uuid>` so it shows up readably in tags and status. |
| Display when uuid is unknown | If a lock/tag references a uuid the local registry has no entry for (e.g., another worktree's agent), the display layer falls back to `agent-<short-uuid>` and notes "(unknown to this host)". Never print a bare uuid as the primary display form. |

## Target canonicalization

Every CLI operation canonicalizes its target argument before any lookup, comparison, or write. The canonical form is what gets hashed for storage paths and what overlap detection compares.

| Rule | Behavior |
|---|---|
| Storage form | Repo-relative POSIX path. The repo root is `git rev-parse --show-toplevel` of the cwd. |
| Path cleaning | `filepath.Clean` semantics: `./a`, `a`, `a/./`, `a//b` → all collapse to a canonical form. |
| Trailing slash | A trailing slash is the type-tag distinguishing form 2 (directory prefix) from form 1 (exact path). Stored verbatim, so the two are **distinct lock keys** for independent-unlock purposes. **But overlap is conservative**: a form-2 target `x/` overlaps the form-1 target `x` and every target below `x/`. So if A holds `internal/store/` and B requests `internal/store`, B is rejected — and vice versa. Independent unlock plus conservative overlap together mean two agents can never each "lock the directory" without seeing each other. **Lock-time warning**: if the user runs `loto lock <path>` and `<path>` exists as a directory on disk, the binary emits a warning that the exact-path form does not protect the subtree, and suggests the trailing-slash form. The warning is loud but non-blocking; literal-spelling intent is still honored. |
| Repo escape | Targets resolving outside the repo (`../../etc/passwd`) are rejected, exit 2. |
| Symlinks | Not resolved. The literal path is what gets locked. Two symlinked names for the same file are two distinct targets — the user is responsible for picking one. (We never touch the on-disk content; we only coordinate intent.) |
| Case sensitivity | Targets are stored case-sensitive (distinct lock keys for independent unlock), but **overlap is filesystem-aware**. On binary start, the project state directory is probed once for case-sensitivity (via a `tmp` + `tmp` create-then-stat-other-case test); the result is cached in `schema_meta` for the project. On a case-insensitive filesystem (macOS APFS default, exFAT), overlap detection treats case variants as overlapping — so locking `Foo.go` rejects a concurrent `foo.go` lock by another agent. On a case-sensitive filesystem (most Linux), case variants are treated as distinct paths. `doctor` reports both the detected mode and any case-variant pairs as findings. The detection-then-cache strategy avoids racing the probe per invocation and lets the operator override via `schema_meta` if the auto-detect is wrong. |
| Glob preservation | Glob meta-characters (`*`, `**`, `?`, `[...]`, `{...}`) are preserved verbatim through canonicalization. `./internal/**/foo.go` and `internal/**/foo.go` canonicalize to the same target. |
| Windows | Out of scope. `loto` is Unix-first per non-goals. |

`unlock <target>` releases the lock whose canonical target equals the requested canonical target. `unlock internal/store/store.go` and `unlock ./internal/store/store.go` refer to the same lock; `unlock internal/store/**` and `unlock internal/store/store.go` remain distinct (independent-unlock rule preserved).

## CLI surface

```
loto whoami
loto lock <target> [--ttl 30m] [--intent "..."]
loto unlock <target>
loto unlock --session                        # this session's locks (used by SessionEnd hook)
loto unlock --all-mine                       # every lock owned by my uuid (manual escape)
loto break <target> --reason "..."           # stale/dead-pid only
loto break <target> --force --reason "..."   # live takeover allowed

loto tag <target> [--to <agent>] [--ttl 1h] "<note>"
loto untag <target> <tag-id>

loto status [<target>...]                    # default: project-wide
loto status <target>                         # diagnostic: why is this clear/blocked/stale?
loto status --mine                           # locks owned by my uuid
loto status --session                        # locks created by this session
loto inbox [--mine] [--unread] [--mark-read]
loto check-paths <target>...
loto check-paths --staged

loto doctor [--repair] [--dry-run]
```

`refresh` is **not** a separate verb. Re-issuing `lock` against a target you already own refreshes intent and TTL atomically; this is the only way to extend a held lock. The verb count stays small.

**Read-only commands never mutate state.** `status`, `inbox`, `check-paths`, `whoami`, and bare `doctor` are pure reads. They surface stale or dead-pid locks in their output but do not reclaim. Reclamation belongs to `lock`, `break`, and `doctor --repair`.

`unlock --session` releases every lock with a `session_uuid` matching this session. Used by the SessionEnd hook so cleanup is precisely scoped to the dying session, not to every lock the agent's uuid has ever placed. `unlock --all-mine` is the broader manual escape — useful when an agent identity persists across multiple shells. `--session` is preferred for automation. (Under the current identity model — one Claude session = new uuid at SessionStart — `--session` and `--all-mine` release the same set; keeping them distinct preserves the abstraction if identities ever persist across sessions.)

**Addressee resolution accepts both forms.** Anywhere a tag addressee is named (e.g. `tag --to`), the CLI resolves either a handle (display form) or a uuid (canonical form) at write time. Agents commonly see uuids in holder reports for sessions whose handle is unknown to this host; the address path must accept those uuids without a workaround. The exact flag shape is a CLI ergonomics choice; the principle is what's load-bearing.

`status <target>` is the diagnostic command — the answer to "why can't I touch this?". Output includes: the exact lock at this target if any (live, stale, dead-pid); overlapping locks owned by others; tags on this target (own, addressed-to-me, system-authored); tags on overlapping targets relevant to me. It is the one command an agent runs when blocked.

Both `status` and `doctor` print a project identity header so a user reading the output across worktrees can orient quickly:

```
project: dkoosis-loto
repo:    /Users/dk/Projects/loto
state:   ~/.local/state/loto/projects/dkoosis-loto
```

Bare `loto doctor` with no flags is a read-only audit: prints stale locks, dead-pid holders, schema drift. `--repair` applies safe fixes (delete stale rows, write reclaim system tags, vacuum the SQLite database); `--dry-run` shows what `--repair` would do without touching disk.

`loto check-paths --staged` reads `git diff --cached --name-only -z` from the repo containing the cwd; renames are checked at both source and destination paths; submodules and untracked files are skipped. Refuses (exit 1) if any staged path overlaps another agent's lock. (`--no-verify` bypasses the git pre-commit hook by design; loto is not a security boundary against a determined operator.)

LLM-format output follows `.claude/rules/design.md`: glyphs over severity words, deterministic sort, paths relative to cwd where possible, no ANSI, explicit empty-status headers. v2 emitters must conform; this rule is load-bearing for the agent stdout audience.

Default output is the LLM format (terse, line-oriented, `loto:llm:v1` header) when stdout is not a tty. `--json` forces JSON. Exit codes are stable:

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | lock conflict (someone else holds an overlapping lock) |
| 2 | usage error |
| 3 | IO / system error |

`try` is removed as a CLI verb. Its in-process flock semantics may remain in the Go library if any caller needs an atomic critical section within one process, but the CLI exposes only `lock` (with a TTL'd persistent claim) and `break` (forced reclaim).

## Lock overlap

Overlap detection is conservative — false positives are tolerable (lock attempt rejected when it could have proceeded), false negatives are not (two agents' edits collide). Specifically: (a) form-2 dir prefix `x/` overlaps form-1 exact `x` and every target below `x/`; (b) on a case-insensitive filesystem, case variants of the same path overlap; (c) form-3 glob overlap uses the `patternsOverlap` helper from v1, which already returns the right answer for the cases that matter (identical pattern, prefix containment, glob-matches-glob). The release-blocking grammar test matrix in the implementation notes pins these down.

Overlap rules:

| Existing | Attempt | Result |
|---|---|---|
| same target, same agent | re-`lock` | idempotent refresh of intent/TTL, exit 0 |
| overlapping target, same agent | new `lock` | exit 0, separate lock placed — locks stack (a refinement, not a conflict; same agent can hold both `internal/store/**` and `internal/store/store.go`) |
| overlapping target, different agent | new `lock` | exit 1, holder report |
| non-overlapping target | new `lock` | exit 0, lock placed |

`loto lock '**'` is the equivalent of v1's global lock: it succeeds only if **no other agent's** locks exist (you can already hold narrower locks of your own; you just can't override anyone else). `**` overlaps every other agent's possible target by definition.

**Independent unlock.** Each lock is a separate record. `unlock <target>` releases only the lock at that exact target string; overlapping locks owned by the same agent are independent and survive. So if A holds both `internal/store/**` and `internal/store/store.go`, `unlock internal/store/**` leaves the narrower one in place. This rule is intentional — it prevents `unlock`-on-a-glob from silently dropping coverage of paths the agent never named.

**Dead-pid detection.** A lock carries `(host, pid)`. When evaluating staleness, if `host == this host` and `kill(pid, 0)` reports the pid is not running, the lock is **stale** regardless of its TTL. Stale locks are surfaced by reads (`status`) and reclaimed by writes (`lock`, `break`, `doctor --repair`) — never by reads themselves. Reclamation INSERTs a system tag and DELETEs the stale row in one transaction. This restores the v1 `--hold` crash-recovery behavior that flock provided automatically. (Cross-host pid checks are out of scope per non-goals. **Hostname-change footnote:** locks placed under a hostname that no longer matches `os.Hostname()` — VM clones, container restarts, laptop renames — fall through dead-pid reclaim and persist until TTL expiry. Acceptable; documented so a future debugger isn't surprised.)

Overlapping tags are always allowed (tags don't enforce); on `tag add`, the binary surfaces existing tags on the same or overlapping targets as a `⚠ overlaps existing` block in the response so the author sees who else is in the area.

## Persistence

### Layout

```
$XDG_STATE_HOME/loto/                              # canonical, shared across worktrees
└── projects/<project-slug>/                       # one per logical project (git remote-derived)
    ├── loto.db                                    # SQLite, WAL mode — locks, tags, schema metadata
    ├── loto.db-wal                                # SQLite WAL file (managed by SQLite)
    └── loto.db-shm                                # SQLite shared-memory index (managed by SQLite)

~/.loto/agents/
    └── <uuid>.json                                # host-global session identity record
```

The backing store is **SQLite in WAL mode**, accessed via the pure-Go `modernc.org/sqlite` driver (no CGO). SQLite is used not because loto is a database-shaped application, but because the central invariant is transactional: observe the lock set, decide overlap, and mutate the lock/tag state as one serialized operation. The alternative would be homemade concurrency code (project mutex + per-file flock + tmp+rename + JSONL append discipline), which v1 attempted; SQLite WAL retires that machinery wholesale.

SQLite WAL natively provides:

- Concurrent readers that don't block writers
- Serialized writers via a single `BEGIN IMMEDIATE` transaction
- Atomic multi-row writes (e.g., break-takeover writes the system tag and replaces the lock in one transaction)
- Crash-safe durability without manual `fsync`/`tmp+rename` choreography
- Range queries for overlap and `status` filters

This replaces the v1 design's project-mutex + per-target tag-flock + tmp+rename + JSONL-append-with-PIPE_BUF dance. The Concurrency contract bullets describe the guarantees; SQLite WAL is the mechanism that delivers them.

Trade: lose `cat`-style debugging of per-target files. Mitigated by `loto status <target>` (the diagnostic command) covering the human-inspect case; `sqlite3 loto.db` is available for forensics. Identity records stay as host-global JSON files (they're genuinely per-agent, host-scoped, never project-scoped). `modernc.org/sqlite` adds ~2–3 MB to the binary vs the v1 no-DB design; acceptable cost for the coordination guarantees.

**Project slug derivation.** Slug = `<owner>-<repo>` from the first `git remote get-url origin` host-path; falls back to `local-<sha256(repo-toplevel-abspath)[:8]>` when no `origin` remote exists. Multiple remotes use `origin` regardless of order. Detached worktrees and submodules resolve via `git rev-parse --show-toplevel` of the cwd. The exact derivation must match v1's existing implementation (see the existing `projectSlug` in `cmd/loto/base.go`); the v2 spec adopts v1's behavior verbatim. Verify the symbol still resolves at implementation time — package layout has been churning.

### Schemas

SQLite DDL (canonical):

```sql
CREATE TABLE locks (
  target_canonical TEXT PRIMARY KEY,    -- canonical target string (file path, dir/, or glob)
  target_kind      TEXT NOT NULL,       -- 'file' | 'dir' | 'glob'
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,    -- unix nanoseconds
  expires_at       INTEGER NOT NULL,    -- unix nanoseconds
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  branch           TEXT NOT NULL DEFAULT ''  -- display-only
);
CREATE INDEX idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX idx_locks_session  ON locks(session_uuid);
CREATE INDEX idx_locks_expires  ON locks(expires_at);

CREATE TABLE tags (
  target_canonical    TEXT NOT NULL,
  id                  TEXT NOT NULL,            -- 't-<8-hex>' (see Tag id)
  kind                TEXT NOT NULL,            -- 'note' | 'system'
  event               TEXT,                     -- non-null when kind='system' (e.g. 'lock_broken')
  author_uuid         TEXT NOT NULL,
  addressee_uuid      TEXT,
  previous_owner_uuid TEXT,                     -- non-null for system tags about a prior owner
  intent              TEXT NOT NULL,
  created_at          INTEGER NOT NULL,         -- unix nanoseconds
  expires_at          INTEGER,                  -- nullable; non-null = expirable
  PRIMARY KEY (target_canonical, id)            -- per-target uniqueness; collision math is per-target, not global
);
CREATE INDEX idx_tags_target     ON tags(target_canonical, created_at);
CREATE INDEX idx_tags_addressee  ON tags(addressee_uuid, created_at);
CREATE INDEX idx_tags_expires    ON tags(expires_at);

CREATE TABLE read_cursors (
  agent_uuid       TEXT NOT NULL,
  target_canonical TEXT NOT NULL,
  last_read_at     INTEGER NOT NULL,             -- unix nanoseconds; max created_at this agent has acknowledged on this target
  PRIMARY KEY (agent_uuid, target_canonical)
);
CREATE INDEX idx_cursors_agent ON read_cursors(agent_uuid);

CREATE TABLE schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- schema_version, created_at, fs_case_sensitive, etc.
```

Cursor lives in the project DB, not in `~/.loto/agents/`, because it is per-(agent, project, target) — not host-global agent state. Putting it here also lets `inbox --mark-read` advance the cursor and read tags in the same `BEGIN IMMEDIATE` transaction, so a crash mid-update can't tear.

Logical-record view (what gets *recorded*, regardless of column layout):

**Lock** logical record:

```json
{
  "owner_uuid": "9e3c1e54-...",
  "session_uuid": "f70a3b22-...",
  "target": "internal/store/store.go",
  "target_kind": "file",
  "intent": "store refactor — beads loto-7wp.4",
  "created_at": "2026-05-10T07:14:11Z",
  "expires_at": "2026-05-10T07:44:11Z",
  "host": "dk-mac",
  "pid": 84231,
  "branch": "store-refactor"
}
```

`session_uuid` is set at SessionStart by the same hook that exports `LOTO_AGENT_ID`. With the current identity model (one Claude session = new uuid per SessionStart), `session_uuid` is effectively redundant with `owner_uuid` — but recording it explicitly future-proofs the SessionEnd hook against any later change to identity persistence. `branch` is **display-only**: shown in holder reports for orientation, never used for overlap, takeover, or any safety decision.

**Tag** logical record (one row in the `tags` table):

```json
{
  "id": "t-9c4f...",
  "kind": "note",
  "author_uuid": "9e3c1e54-...",
  "target": "internal/store/store.go",
  "intent": "ping me when you're free",
  "addressee_uuid": "1f48b9a0-...",
  "created_at": "2026-05-10T07:18:02Z",
  "expires_at": null
}
```

System-authored tag (e.g. break-takeover):

```json
{
  "id": "t-3e1a...",
  "kind": "system",
  "event": "lock_broken",
  "author_uuid": "1f48b9a0-...",
  "previous_owner_uuid": "9e3c1e54-...",
  "addressee_uuid": "9e3c1e54-...",
  "target": "internal/store/store.go",
  "intent": "deadline — store refactor must merge by 14:00",
  "created_at": "2026-05-10T07:21:00Z",
  "expires_at": null
}
```

**Tag id** is `t-<short-hash>` where short-hash = first 8 hex chars of `sha256(author_uuid || created_at_unix_nano || intent)`. Stable, user-quotable, and per-target unique by virtue of the composite primary key `(target_canonical, id)`. The 8-hex collision space is sized against the per-target tag log (small — tens to hundreds at most), not the global tag count, so the negligible-collision claim and the storage shape now agree. INSERT conflicts on the composite PK are not expected in practice; the binary surfaces them as exit 3 (IO/system error) rather than papering over with a salted retry.

**Read cursor** is stored as `read_cursors(agent_uuid, target_canonical, last_read_at)` rows in the project DB (DDL above). One row per (agent, target). `inbox --unread` returns addressed, non-expired tags whose `created_at` is later than `last_read_at` for the agent on that target — pure SELECT, no cursor mutation. `inbox --mark-read` advances `last_read_at` to the latest `created_at` seen, in the same `BEGIN IMMEDIATE` transaction as the read so the cursor advance can't tear against concurrent tag inserts. If no row exists for a (agent, target) pair, all addressed non-expired tags on that target are unread.

**Agent identity** (`~/.loto/agents/<uuid>.json`):

```json
{
  "uuid": "9e3c1e54-...",
  "handle": "FullHorse",
  "created_at": "2026-05-10T06:00:00Z",
  "host": "dk-mac"
}
```

### Garbage collection

- Stale locks (past TTL or with a dead pid on this host) are reclaimed by `lock` (when acquiring the same target), by `break`, and by `doctor --repair`. Reclamation INSERTs a system tag explaining the reclaim and DELETEs the stale row in the same SQLite transaction. **Read-only commands surface stale rows in output but never delete them.**
- Expired tags (rows with `expires_at IS NOT NULL AND expires_at < now()`) are deleted lazily by `doctor --repair`, or opportunistically by any write that already holds the SQLite writer lock. Reads filter expired tags via `WHERE expires_at IS NULL OR expires_at > now()`.
- **Adhoc identity prune.** `~/.loto/agents/<uuid>.json` records accumulate one-per-invocation when `bash -c "loto …"` runs without `LOTO_AGENT_ID` (cron, scheduled tasks, manual shells). `doctor --repair` deletes any agent record whose uuid has no live lock and no recent (within last 30d) authored tag in any project DB on this host. Read-only `doctor` lists candidates without removing them.
- **DB integrity.** `doctor` runs `PRAGMA integrity_check` and reports the result. On failure, `doctor --repair` **moves the corrupt file aside** (`loto.db.corrupt.<RFC3339-Z>`) and creates a fresh DB. Forensic recovery stays possible (open the moved-aside file with `sqlite3` and salvage what's recoverable); the active project DB returns to a clean known state. v1 file-per-target localized corruption to one record; v2 trades that for transactional safety, so the repair path has to handle whole-DB loss without feeling cavalier about it.
- No daemon, no sweep, no application-level background work. SQLite's `PRAGMA wal_autocheckpoint` handles WAL checkpointing automatically.

## Concurrency contract

The contract — what callers can rely on. Mechanics in the Implementation Notes appendix.

1. **All state-mutating operations execute as a single SQLite transaction (`BEGIN IMMEDIATE` … `COMMIT`).** This applies to `lock`, `unlock`, `break`, `tag`, `untag`, and `doctor --repair`. WAL mode allows concurrent readers; writers serialize on the database file. Two concurrent `lock` invocations on overlapping-but-distinct targets are forced into sequence; the second observes the first's row and either rejects (different agent) or refines (same agent). The central safety claim — no concurrent acquisition of overlapping locks — is enforced by SQLite's writer-serialization, not by application-level mutex gymnastics.
2. **Atomicity is transactional.** Multi-row writes commit together or not at all. Break-takeover (write system tag + replace lock) and stale-reclaim (write system tag + delete row + insert new row) are each one transaction. There is no observable state where a system tag exists without its corresponding lock change, or vice versa. Earlier drafts' "tag-first ordering" rule is obsolete — both writes are in one transaction.
3. **Read-only commands take no write locks and mutate no state.** `status`, `inbox`, `check-paths`, `whoami`, bare `doctor` are pure SELECTs. They surface stale or expired rows in their output but never DELETE.
4. **Refresh verifies ownership atomically.** Re-issuing `lock` against an owned target is `UPDATE locks SET intent = ?, expires_at = ? WHERE target_canonical = ? AND owner_uuid = ?`. A refresh that races a `break --force` either commits before the break (refresh wins; break then sees the refreshed row) or after (refresh's WHERE clause matches zero rows because owner changed; binary returns exit 1, "lock no longer mine"). A refresh cannot resurrect a broken lock.
5. **Reads observe a consistent snapshot.** WAL mode gives readers a single point-in-time view of the database. `loto status` cannot show inconsistent state across the lock and tag tables.
6. **Overlap detection correctness is release-blocking.** Forms 1 (exact path) and 2 (directory prefix) have trivial overlap rules — string equality and path-prefix comparison. Form 3 (globs) uses `patternsOverlap`; v2 ships only after an exhaustive grammar test matrix covering identical patterns, prefix-containment, glob-vs-glob, glob-vs-literal, dot-segments, and brace expansion if supported. Any documented false-negative blocks release — false-negative means silent overlap-collision, which violates the central lock contract. False-positives (rejecting safe overlap) are tolerable.
7. **Domain decisions are pure functions.** Overlap detection, TTL/staleness checks, takeover authorization, and tag-relevance filtering are pure functions over their inputs. They are testable without any database, filesystem, or environment dependency. The SQLite layer, CLI parser, and hook wrappers are adapters around this pure core; the core never imports them.

## Behavioral enforcement

**Cut rule.** *Binary owns invariants enforceable by exit code on a single invocation. Hooks own behaviors that require participation in Claude's tool-use loop. Skills are read by humans only when neither of the above applies.*

The table below is verification of the rule, not the source of it.

| Behavior to enforce | Where it lives | What it does |
|---|---|---|
| Release this session's locks on session end | `SessionEnd` hook | runs `loto unlock --session` (precisely scoped — does not release locks placed by other sessions sharing the agent uuid, if any). `--all-mine` is the manual broader escape, never automatic. |
| Don't edit a path locked by another agent | `PreToolUse` on Edit/Write/MultiEdit | wrapper runs `loto check-paths` against the tool input. Binary returns exit 1 (lock conflict) with a holder report; the wrapper translates exit 1 → exit 2 (Claude Code "block this tool call"). The binary's exit-code table is not bent for hook needs — translation lives in the hook wrapper. |
| Don't commit changes that touch another agent's locked paths | git `pre-commit` hook | runs `loto check-paths --staged`; refuses the commit on conflict |
| Surface addressed tags + want-next on lock acquire | **binary** | `loto lock` success output includes any tags on the target where `addressee_uuid == me` or where the tag is unaddressed-but-relevant (prior holder's heads-up). Read cursor advances. |
| Surface relevant tags on lock release | **binary** | `loto unlock` reads tags on the target before returning; if any have an addressee, prints a `→ notify` block listing each so the releasing agent sees who's waiting |
| Periodic inbox check during a long session | `Stop` hook + binary | binary already surfaces on every lock/unlock; hook covers sessions with no lock activity. See concrete shape below. |
| `break <target> --reason` reclaims stale only | **binary** | exit 1 if the target's lock is live; the recovery path stays safe by default |
| `break --force --reason` permits live takeover | **binary** | system-authored tag (`kind: system`, `event: lock_broken`, truthful `author_uuid`/`previous_owner_uuid`) is inserted in the same transaction that replaces the lock; observers never see one without the other; `--reason` is required for both forms |
| Same-agent same-target re-lock | **binary** | idempotent refresh of intent/TTL via `UPDATE … WHERE owner_uuid = ?` inside a `BEGIN IMMEDIATE` transaction; SQLite writer serialization (not any application-level mutex) is what orders refresh against `break` |
| Tag id stable + author-only `untag` | **binary** | id = `t-<8-hex of sha256(author_uuid‖created_at_nano‖intent)>`; `untag` rejects (exit 1) if author_uuid != me, with `doctor --repair` as the GC escape |

The pre-edit and pre-commit hooks are the load-bearing safety net. They turn "remember to check before editing" (paper shield) into an enforced exit-2 refusal.

`--no-verify` bypasses the git pre-commit hook by design; loto is not a security boundary against a determined operator. Trust model = trust the operator (carried from NS).

### Stop hook concrete shape

Claude Code's `Stop` hook receives the assistant's final response and may emit JSON on stdout to inject context into the next prompt. The loto Stop hook:

```bash
#!/bin/sh
# Installed by `loto install-hook --stop`. Runs as the Stop hook.
unread=$(loto inbox --mine --unread --json 2>/dev/null) || exit 0
test -z "$unread" -o "$unread" = "[]" && exit 0
printf '{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":%s}}\n' \
  "$(loto inbox --mine --unread --format llm 2>/dev/null | jq -Rs .)"
```

`additionalContext` is the field name in the current Claude Code hook protocol (Claude Code 1.x). The hook does **not** call `--mark-read` — Claude marks tags read by acting on them or via `loto inbox --mark-read` explicitly. This avoids losing visibility on tags Claude has seen but not yet addressed.

## Skill

Drop the existing `loto` skill, or shrink it to a trigger paragraph that points at `loto --help`. The binary's CLI help and the hooks together carry the protocol; the skill becomes documentation of last resort, used only when a behavior cannot be hooked.

A non-goal: skills as a substitute for missing binary or hook capabilities. If a skill is teaching agents to do something the binary should refuse or the hook should enforce, the gap is in the binary or hook — file it, fix it, delete the skill section.

## Non-goals

Carried forward from NS:

- Multi-host coordination (NFS, network shares — flock semantics break, and we don't rely on flock anyway, but the underlying assumption of one filesystem stands)
- Daemons or long-running processes
- Strong consistency or transactions across multiple targets
- Replacing review, tests, or human judgment
- Solving git conflicts (loto reduces them; git resolves them)
- Permissions (any agent can `break --force` any lock with a reason; trust model = trust the operator)
- Gating reads (loto coordinates writes only)

New:

- **Multi-lock hasp.** A target has at most one lock-holder. Cooperation across agents on the same target uses tags, not co-locking.

## v1 → v2 surface mapping

v1 coordination state is ephemeral (short-TTL locks, per-session identities, conversational mailboxes) and is not migrated. v2 starts each project with a fresh SQLite database; sessions running v1 at the moment of upgrade simply lose their (already short-lived) coordination state, the same way they would on a `rm -rf ~/.local/state/loto/`. Users who want to clean up v1 files after upgrade can `rm` them manually.

What changes at the CLI:

| v1 concept | v2 mapping |
|---|---|
| `loto try file <path> [--hold]` | `loto lock <path> [--ttl ...]`. Foreground hold (`--hold`) becomes "lock with a TTL and refresh while you're working." |
| `loto try global` | `loto lock '**'` — globs already cover this. v1's separate `global.lock` is just a lock on `**` in v2. |
| `loto reserve add <glob>` | `loto lock <glob>` (when claiming territory) **or** `loto tag <glob> --intent "..."` (when only declaring). Both paths exist; the agent picks based on whether they want exclusion. |
| `loto reserve list` | `loto status` (locks) + `loto inbox` (tags addressed to me) |
| `loto msg <target> --to <agent> "..."` | `loto tag <target> --to <agent> "..."` |
| `loto inbox <target>` / `loto inbox --mine` | unchanged in shape; reads from the new SQLite tag table |
| `loto break <target>` (reap-only) | `loto break <target> --reason "..."` (stale only); `loto break --force --reason "..."` adds the live-takeover capability |
| `loto check-paths` | unchanged in shape; consults the SQLite locks table (overlap) and tags table (informational) |
| `loto install-hook` | unchanged surface; PreToolUse pre-edit gate added; SessionEnd uses `unlock --session` |
| `loto install-git-hook` | unchanged |

## Deferred (explicitly out of scope for v2)

To stay a sharp hand-tool rather than infrastructure, these are deliberately *not* in v2 even though some have come up in conversation. Adding any of them is a separate spec.

- `loto explain <target>` — `status <target>` covers the use case
- `loto lock --staged` / `--modified` — convenience over `git diff` parsing
- Real queues / auto-promotion ("you got it next") — `kind: note` addressed tags cover the human-loop case
- Watchers, notifications, daemon behavior, cron triggers
- Lock priority, lock reservations, multi-lock hasps
- Branch-scoped locks (branch is display-only; see Lock primitive)
- Cross-host coordination
- Permissions / authorization beyond owner-only release and `--force --reason`
- Rich tag taxonomies beyond `note | system`
- Prepared statements / templates for common tag intents
- MCP server adapter for agent orchestration. The CLI + hooks combo serves agents adequately (`loto lock; if [ $? -eq 1 ]; then ...`); a typed MCP surface would double maintenance for no v2 benefit. Legitimate v3 territory if CLI ergonomics ever prove insufficient.
- Markdown / YAML frontmatter for tag bodies. Tags are short notes; SQLite-stored TEXT is the right shape. Multiline rendering is a downstream display concern, not a storage one.

## Acceptance

A fresh Claude dropped into any worktree of a project where four other Claudes are working can:

1. Run `loto status` and understand who's locked what in <1s, with a project-identity header so cross-worktree orientation is immediate.
2. `loto lock <path>` with confidence that overlap is a hard refusal, not a silent overwrite — including under concurrent invocation by another agent on an overlapping target.
3. Receive a useful holder report when blocked — every overlapping blocker listed in deterministic order, each with held-since, TTL, intent, host, branch (display only).
4. Receive any addressed tags / want-next signals automatically on every `lock` and `unlock`, without remembering to check.
5. Tag a held file with "ping me when you're free" and trust the holder will see it on their next `unlock`.
6. Crash, restart, and resume — stale locks reclaim on the next `lock`/`break`/`doctor --repair`; no human cleanup, and `status` never silently mutates.
7. Have edits to another agent's locked paths refused at `PreToolUse`, before any disk write happens.
8. Two agents cannot concurrently acquire overlapping locks, even when `loto lock` is invoked at the same moment on different targets. (Mechanism lives in implementation notes; acceptance is behavioral.)
9. Equivalent path spellings normalize to the same target before lock, unlock, status, check-paths, and tag lookup.
10. Read-only commands (`status`, `inbox`, `check-paths`, `whoami`, bare `doctor`) never mutate lock or tag state.
11. `loto status <target>` makes it obvious why a target is clear, blocked, stale, or socially annotated.
12. `loto status --mine` and `loto status --session` make it obvious what cleanup will release.
13. A live lock break is visibly exceptional (`--force` required) and leaves a truthful system-authored audit tag (`kind: system`, `author_uuid` = breaker) on the target.
14. SessionEnd cleanup releases only locks created by the dying session, not every lock owned by the same agent uuid.

Any **write-safety invariant** described in this spec that the binary or hook does not enforce by exit code is a bug. Anything the binary or hook enforces that this spec doesn't describe is a misalignment to fix in one direction or the other. Tags are informational and intentionally do not enforce.

## Implementation notes

Mechanics that support the Concurrency contract; not part of the contract itself but load-bearing for any implementer.

### SQLite configuration

- Driver: `modernc.org/sqlite` (pure Go, no CGO).
- On first open, the binary runs:
  ```sql
  PRAGMA journal_mode = WAL;
  PRAGMA synchronous  = NORMAL;
  PRAGMA busy_timeout = 5000;       -- ms; tolerate brief writer contention
  PRAGMA wal_autocheckpoint = 1000; -- pages; default behavior
  ```
- `journal_mode = WAL` is per-database and persists; the PRAGMA is idempotent.
- `synchronous = NORMAL` is the WAL-recommended setting — durable enough for our use (loss of last few transactions on power failure is acceptable; we're not a financial ledger), much faster than `FULL`.
- `busy_timeout = 5000` means a writer waiting for another writer retries internally for up to 5s before returning `SQLITE_BUSY`. Practical write critical sections are sub-millisecond, so this only fires under genuine contention or stuck processes.

### Schema management

- `schema_meta` table holds `schema_version` (integer, monotonically increasing).
- On binary start, compare on-disk version to the binary's known version. Newer on-disk → exit 3 with "loto.db is from a newer loto version; upgrade or remove the database." Older on-disk → run forward migrations sequentially in one transaction each.
- Schema migrations live as `migrations/NNN_description.sql` in the binary (embedded via `embed.FS`). These are forward-only schema evolutions for v2 itself; v1 file-based state is not migrated (see "v1 → v2 surface mapping").

### Domain core (pure functions)

Per Concurrency contract item 7. The following live in a `domain` package with no imports of `database/sql`, `os`, `path/filepath`, or anything else with side effects. They take and return plain Go values.

- `Overlap(a, b Target) bool` — pure overlap check across the three target forms (file/dir/glob).
- `IsStale(lock LockRecord, now time.Time, hostPidLive func(host string, pid int) bool) bool` — TTL plus dead-pid check; the dead-pid probe is injected as a function so tests pass a fake.
- `AuthorizeUnlock(lock LockRecord, byAgent uuid.UUID) error` — owner-only release rule.
- `AuthorizeBreak(lock LockRecord, byAgent uuid.UUID, force bool, now time.Time) error` — stale-vs-live distinction, `--force` requirement.
- `RelevantTags(tags []TagRecord, forAgent uuid.UUID, target Target, kind RelevanceKind) []TagRecord` — filter for `lock`/`unlock` surfacing and `inbox` queries.

The SQLite adapter, CLI parser, and hook wrappers compose around this core. Tests of the core do not touch disk or shell.

### Read cursor

- Stored as `read_cursors` rows in the project DB; advances atomically with the underlying read in one `BEGIN IMMEDIATE` transaction. No tmp+rename, no last-write-wins window — the cursor cannot tear against concurrent tag inserts.
- An agent's cursor for a target is namespaced by `agent_uuid`, so two agents reading the same target keep independent positions.
- An earlier draft kept the cursor as `~/.loto/agents/<uuid>/read-cursor.json`. That mis-scoped the data: a cursor entry is per-(agent, project, target), so storing it under the per-agent host-global directory caused cross-project key collisions when the same target path appeared in multiple projects. Project DB storage fixes this and removes a JSON file from the layout.

### Hook script details

The Stop hook (see Behavioral enforcement) emits JSON with `additionalContext` per the Claude Code 1.x hook protocol. The PreToolUse hook wrapper translates the binary's exit 1 (lock conflict) into the hook protocol's exit 2 (block tool call). Neither hook bends the binary's exit-code table; mapping lives in the wrapper.

### Patterns overlap test matrix

The release-blocking test matrix (Concurrency contract item 6) lives in the `domain` package's `overlap_test.go`. At minimum it must cover: identical patterns; prefix containment in both directions; `**`-vs-literal; `**`-vs-`**`; literal-vs-literal disjoint; literal-vs-glob with shared literal prefix; brace expansion if our doublestar dependency supports it; dot-segment edge cases; **form-2 dir-prefix `x/` vs form-1 exact `x`** (must overlap); **form-2 dir-prefix `x/` vs nested file `x/a/b.go`** (must overlap); case variants under both case-sensitive and case-insensitive filesystem modes; mixed file/dir/glob target forms. The bar is **zero documented false-negatives**; any false-negative blocks v2 release.
