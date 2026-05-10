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

Exclusive claim by one agent on one target. Target may be a file path, a directory, or a glob — whatever scope the agent needs to touch.

- **Single owner.** Only the placing agent releases.
- **Overlap blocks.** A `lock` attempt fails (exit 1) if its target overlaps any existing lock owned by a different agent. Overlap is symmetric: any path that could match both targets counts. Same-agent re-lock is idempotent (refreshes intent/TTL in place).
- **TTL.** Every lock carries an expiry. Default `--ttl` is `30m` when not specified — long enough for a typical edit session, short enough that crashed agents don't strand locks for hours. The owner re-issues `lock` against the same target to extend (idempotent refresh). Past TTL the lock is *stale*: it still occupies the slot until reclaimed.
- **Forced takeover.** `break --force --reason "..."` lets another agent reclaim a stale-or-live lock; the binary writes a tag in the displaced owner's name on the same target naming the breaker and the reason. No silent dispossession, ever.
- **Persistence.** Stored as a JSON file under `$XDG_STATE_HOME/loto/projects/<slug>/locks/`, owner-stamped, atomically written via tmp+rename.

The blocked-attempt response carries a structured holder report:

```json
{
  "blocked_by": "GreenCastle",
  "intent": "store refactor — beads loto-7wp.4",
  "target": "internal/store/store.go",
  "held_since": "2026-05-10T07:14:11Z",
  "expires_at": "2026-05-10T07:24:11Z",
  "host": "dk-mac",
  "pid": 84231
}
```

### Tag

A note attached to a target. Multiple tags may attach to the same target. Each tag carries:

- author (agent handle)
- target (file/dir/glob — same shape as lock targets)
- intent (free-text note)
- optional addressee (another agent handle)
- optional TTL (expired tags are eligible for prune)

Tags are informational only. They neither block nor grant access. Use cases:

| Use case | Shape |
|---|---|
| "I'm working on X, done by 14:00" | tag on file/glob, no addressee, TTL ≈ work duration |
| "Ping me when you're free with this file" | tag on file, addressee = lock-holder |
| "I broke the test on line 40 before releasing" | tag on file, no addressee, short TTL |
| Direct message to another agent | tag with addressee = recipient |
| Territory declaration ("I'm sweeping internal/store/**") | tag on glob, no addressee |
| System notice ("BlueOak broke your lock — reason: deadline") | tag with addressee = displaced owner |

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

## CLI surface

```
loto whoami
loto lock <target> [--ttl 30m] [--intent "..."]
loto unlock <target>
loto unlock --all-mine
loto break <target> --force --reason "..."

loto tag <target> [--to <agent>] [--ttl 1h] "<note>"
loto untag <target> <tag-id>

loto status [<target>...]
loto inbox [--mine] [--unread] [--mark-read]
loto check-paths <target>...
loto check-paths --staged

loto doctor [--repair] [--dry-run] [--migrate-v1]
```

`refresh` is **not** a separate verb. Re-issuing `lock` against a target you already own refreshes intent and TTL atomically; this is the only way to extend a held lock. The verb count stays small.

`unlock --all-mine` releases every lock owned by `LOTO_AGENT_ID`. Used by the SessionEnd hook.

Bare `loto doctor` with no flags is a read-only audit: prints stale locks, dead-pid holders, orphaned files, layout drift. `--repair` applies safe fixes; `--dry-run` shows what `--repair` would do without touching disk; `--migrate-v1` runs the one-shot v1→v2 on-disk conversion described in the migration section.

`loto check-paths --staged` reads `git diff --cached --name-only -z` from the repo containing the cwd; renames are checked at both source and destination paths; submodules and untracked files are skipped. Refuses (exit 1) if any staged path overlaps another agent's lock.

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

Overlap detection is conservative — false positives are tolerable (lock attempt rejected when it could have proceeded), false negatives are not (two agents' edits collide). The existing `patternsOverlap` helper from v1 is reusable; it already returns the right answer for the cases that matter (identical pattern, prefix containment, glob-matches-glob).

Overlap rules:

| Existing | Attempt | Result |
|---|---|---|
| same target, same agent | re-`lock` | idempotent refresh of intent/TTL, exit 0 |
| overlapping target, same agent | new `lock` | exit 0, separate lock placed — locks stack (a refinement, not a conflict; same agent can hold both `internal/store/**` and `internal/store/store.go`) |
| overlapping target, different agent | new `lock` | exit 1, holder report |
| non-overlapping target | new `lock` | exit 0, lock placed |

`loto lock '**'` is the equivalent of v1's global lock: it succeeds only if **no other agent's** locks exist (you can already hold narrower locks of your own; you just can't override anyone else). `**` overlaps every other agent's possible target by definition.

**Independent unlock.** Each lock is a separate record. `unlock <target>` releases only the lock at that exact target string; overlapping locks owned by the same agent are independent and survive. So if A holds both `internal/store/**` and `internal/store/store.go`, `unlock internal/store/**` leaves the narrower one in place. This rule is intentional — it prevents `unlock`-on-a-glob from silently dropping coverage of paths the agent never named.

**Dead-pid detection on every access.** A lock carries `(host, pid)`. On any read of a lock, if `host == this host` and `kill(pid, 0)` reports the pid is not running, the lock is **stale** regardless of its TTL. The reader treats it the same as a TTL-expired lock — eligible for lazy GC, with a system-tag posted to the original owner's record on reclaim. This restores the v1 `--hold` crash-recovery behavior that flock provided automatically. (Cross-host pid checks are out of scope per non-goals.)

Overlapping tags are always allowed (tags don't enforce); on `tag add`, the binary surfaces existing tags on the same or overlapping targets as a `⚠ overlaps existing` block in the response so the author sees who else is in the area.

## Persistence

### Layout

```
$XDG_STATE_HOME/loto/                              # canonical, shared across worktrees
└── projects/<project-slug>/                       # one per logical project (git remote-derived)
    ├── locks/<sha256(target)>.json                # one per locked target
    ├── tags/<sha256(target)>.jsonl                # append-only tag log per target
    └── tags/<sha256(target)>.lock                 # per-target advisory file lock (POSIX flock)

~/.loto/agents/
    ├── <uuid>.json                                # host-global session identity record
    └── <uuid>/read-cursor.json                    # per-target last-read tag id (this agent)
```

**Project slug derivation.** Slug = `<owner>-<repo>` from the first `git remote get-url origin` host-path; falls back to `local-<sha256(repo-toplevel-abspath)[:8]>` when no `origin` remote exists. Multiple remotes use `origin` regardless of order. Detached worktrees and submodules resolve via `git rev-parse --show-toplevel` of the cwd. The exact derivation must match v1's existing implementation (see `loto.go:projectSlug`); the v2 spec adopts v1's behavior verbatim.

### Schemas

**Lock** (`locks/<sha256(target)>.json`):

```json
{
  "owner_uuid": "9e3c1e54-...",
  "target": "internal/store/store.go",
  "intent": "store refactor — beads loto-7wp.4",
  "created_at": "2026-05-10T07:14:11Z",
  "expires_at": "2026-05-10T07:44:11Z",
  "host": "dk-mac",
  "pid": 84231,
  "branch": "store-refactor"
}
```

**Tag** (one JSONL line in `tags/<sha256(target)>.jsonl`):

```json
{
  "id": "t-9c4f...",
  "author_uuid": "9e3c1e54-...",
  "target": "internal/store/store.go",
  "intent": "ping me when you're free",
  "addressee_uuid": "1f48b9a0-...",
  "created_at": "2026-05-10T07:18:02Z",
  "expires_at": null
}
```

**Tag id** is `t-<short-hash>` where short-hash = first 8 hex chars of `sha256(author_uuid || created_at_unix_nano || intent)`. Stable, user-quotable, and unique-enough for the small number of tags per target. Collision probability for 8-hex within one target's tag log is negligible at any realistic scale.

**Read cursor** (`~/.loto/agents/<uuid>/read-cursor.json`):

```json
{
  "<sha256(target)>": "t-9c4f...",
  "<sha256(target2)>": "t-3e1a..."
}
```

Maps target hash → most-recent tag id this agent has read. `inbox --unread` returns tags newer than the cursor on each target where the agent is the addressee. `inbox --mark-read` advances the cursor to the latest tag id seen.

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

- Stale locks (past TTL or with a dead pid on this host, see overlap section) are pruned by any agent's next access to that target, and by `loto doctor --repair`. Pruning a stale lock posts a system tag in the original owner's name explaining the reclaim.
- Expired tags are pruned during compaction passes triggered when a tag log exceeds 256 lines or 64 KiB, whichever first; compaction runs under the per-target tag lock (see Concurrency).
- No daemon, no sweep, no background work.

## Concurrency & atomicity

This section consolidates the on-disk concurrency contract. All guarantees apply within a single host (per non-goals).

### Lock file operations

- **Create / refresh / break-takeover** — atomic write via `tmp + fsync + rename` to `locks/<hash>.json`. The replace is atomic at the directory entry level on POSIX local filesystems.
- **Refresh ownership check** — re-issuing `lock` against an owned target is "read existing → verify `owner_uuid == me` → write new" performed under a per-target advisory lock on `locks/<hash>.json` (POSIX flock on the file itself). This serializes refresh against `break --force` so a refresh cannot blindly resurrect a lock that was just broken.
- **Break-force ordering**: the displaced-owner tag is **written first** (appended to the tag log), then the lock is atomically replaced. The opposite order would create a window where the lock is gone but the dispossession notice doesn't yet exist — silent dispossession, which the spec forbids. The two writes are not transactional, but the order ensures any visible state has the tag.
- **Dead-pid reclaim** is a special case of break: the reclaimer writes a system tag explaining the reclaim, then atomically replaces the lock. Same ordering rule.

### Tag file operations

- **Append** — per-target JSONL appends are serialized under a per-target advisory file lock (`tags/<hash>.lock`, POSIX flock, exclusive). This removes the PIPE_BUF size constraint on safe appends; a tag with a 32 KiB intent string is fine. Without the lock, POSIX guarantees atomicity only for ≤ PIPE_BUF (4 KiB) `O_APPEND` writes on local filesystems — too narrow to rely on.
- **Compaction** — tag log compaction (drop expired tags, rewrite via tmp+rename) holds the same per-target lock for the duration. A concurrent appender blocks until compaction completes, then appends to the rewritten file. No append is lost.
- **Read** — readers do not take the lock; they tolerate the rewrite via stat-then-reopen on `ENOENT`. JSONL read is line-by-line; partial last-line is dropped (atomic-append guarantee under lock means partial lines only occur if a writer is mid-append, in which case the next read sees the complete line).

### Overlap detection

- **Verification commitment.** v1's `patternsOverlap` is the implementation basis. v2 release is gated on an exhaustive test matrix over the doublestar grammar — at minimum: identical, prefix-containment, glob-vs-glob, glob-vs-literal, dot-segment, brace-expansion if supported. **Any documented false-negative case is a release-blocker**, since false-negative = silent collision (a Tier-0 violation of the lock contract). False-positives (rejecting safe overlap) are tolerable. The test matrix lives at `reservation_test.go`/`patterns_overlap_test.go` and must be reviewed before v2 cuts.

### Inbox / read cursor

- The read cursor is a single JSON file per agent. Updates are atomic via tmp+rename. Concurrent processes for the same agent are not expected (one session = one process running CLI commands serially); if it ever happens, last-write-wins on the cursor file is acceptable — losing a cursor advance just means re-seeing already-read tags, never missing a tag.

## Behavioral enforcement

**Cut rule.** *Binary owns invariants enforceable by exit code on a single invocation. Hooks own behaviors that require participation in Claude's tool-use loop. Skills are read by humans only when neither of the above applies.*

The table below is verification of the rule, not the source of it.

| Behavior to enforce | Where it lives | What it does |
|---|---|---|
| Release my locks on session end | `SessionEnd` hook | runs `loto unlock --all-mine` |
| Don't edit a path locked by another agent | `PreToolUse` on Edit/Write/MultiEdit | runs `loto check-paths` against the tool input; exit 2 with a holder report if any input path overlaps another agent's lock |
| Don't commit changes that touch another agent's locked paths | git `pre-commit` hook | runs `loto check-paths --staged`; refuses the commit on conflict |
| Surface addressed tags + want-next on lock acquire | **binary** | `loto lock` success output includes any tags on the target where `addressee_uuid == me` or where the tag is unaddressed-but-relevant (prior holder's heads-up). Read cursor advances. |
| Surface relevant tags on lock release | **binary** | `loto unlock` reads tags on the target before returning; if any have an addressee, prints a `→ notify` block listing each so the releasing agent sees who's waiting |
| Periodic inbox check during a long session | `Stop` hook + binary | binary already surfaces on every lock/unlock; hook covers sessions with no lock activity. See concrete shape below. |
| `break --force` writes the dispossession tag | **binary** | tag is appended **before** the lock is replaced; `--reason` is required |
| Same-agent same-target re-lock | **binary** | idempotent refresh of intent/TTL, under the per-target lock-file flock to serialize against break |
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

## Migration from v1

| v1 concept | v2 mapping |
|---|---|
| `loto try file <path> [--hold]` | `loto lock <path> [--ttl ...]`. Foreground hold (`--hold`) becomes "lock with a TTL and refresh while you're working." |
| `loto try global` | `loto lock '**'` — globs already cover this. The `global.lock`/`global.tag` files in the layout become an ordinary lock on `**`. |
| `loto reserve add <glob>` | `loto lock <glob>` (when claiming territory) **or** `loto tag <glob> --intent "..."` (when only declaring). Both paths exist; the agent picks based on whether they want exclusion. |
| `loto reserve list` | `loto status` (locks) + `loto inbox` (tags addressed to me) |
| `loto msg <target> --to <agent> "..."` | `loto tag <target> --to <agent> "..."` |
| `loto inbox <target>` / `loto inbox --mine` | unchanged in shape; reads from the new tag store |
| `loto break <target>` (reap-only) | unchanged; `loto break --force --reason "..."` adds the takeover capability |
| `loto check-paths` | unchanged in shape; consults locks (overlap) and tags (informational) |
| `loto install-hook` | unchanged; PreToolUse pre-edit gate added; PostToolUse / SessionEnd release stays |
| `loto install-git-hook` | unchanged |

On-disk migration: the v1 `reservations/` directory contents become v2 tags (intent preserved, no addressee). The v1 `files/<hash>.tag` + `.lock` pair becomes a v2 lock entry. The v1 `files/<hash>.msgs` mailbox becomes per-target tag JSONL with addressee set. A one-shot `loto doctor --migrate-v1` handles the conversion; the layout shape is similar enough that this is mechanical.

## Acceptance

A fresh Claude dropped into any worktree of a project where four other Claudes are working can:

1. Run `loto status` and understand who's locked what in <1s.
2. `loto lock <path>` with confidence that overlap is a hard refusal, not a silent overwrite.
3. Receive a useful holder report when blocked — including held-since, TTL, intent, host.
4. Receive any addressed tags / want-next signals automatically on every `lock` and `unlock`, without remembering to check.
5. Tag a held file with "ping me when you're free" and trust the holder will see it on their next `unlock`.
6. Crash, restart, and resume — stale locks reclaim via TTL + `doctor --repair`; no human cleanup.
7. Have edits to another agent's locked paths refused at `PreToolUse`, before any disk write happens.

Anything in this spec that the binary doesn't enforce by exit code is a bug. Anything the binary enforces that this spec doesn't describe is a misalignment to fix in one direction or the other.
