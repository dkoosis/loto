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
- **Canonical target.** Every operation canonicalizes the target before lookup or write (see Target canonicalization). Equivalent spellings (`./a`, `a`, `a/./`) refer to the same lock; mismatched spellings cannot fragment a lock's identity.
- **Overlap blocks.** A `lock` attempt fails (exit 1) if its canonical target overlaps any existing lock owned by a different agent. Overlap is symmetric: any path that could match both targets counts. Same-agent re-lock is idempotent (refreshes intent/TTL in place).
- **TTL.** Every lock carries an expiry. Default `--ttl` is `30m` when not specified — long enough for a typical edit session, short enough that crashed agents don't strand locks for hours. The owner re-issues `lock` against the same canonical target to extend (idempotent refresh). Past TTL the lock is *stale*: it still occupies the slot until reclaimed.
- **Stale reclaim.** Stale locks (past TTL or with a dead pid on this host) are eligible for reclaim. `lock` reclaims a stale lock as part of acquiring the target (one operation, no separate step). `break` reclaims without acquiring. `doctor --repair` reclaims at scale. Reads — `status`, `inbox`, `check-paths` — never reclaim.
- **Recovery break.** `break <target> --reason "..."` reclaims a *stale* lock and writes a system-authored tag explaining the reclaim. Fails (exit 1) if the lock is live. This is the normal recovery path.
- **Forced live takeover.** `break <target> --force --reason "..."` reclaims a *live* lock owned by another agent. The binary writes a system-authored tag on the same target — `kind: "system"`, `author_uuid = breaker`, `previous_owner_uuid = displaced`, `addressee_uuid = displaced` — naming the breaker and the reason. Tag is appended **before** the lock is replaced. No silent dispossession, ever. No forged authorship, ever.
- **Branch metadata is display-only.** The lock record carries the breaker's branch for orientation; it never affects overlap, takeover, or any other safety decision. Worktrees on different branches still share the same project state and the same overlap rules.
- **Persistence.** Stored as a JSON file under `$XDG_STATE_HOME/loto/projects/<slug>/locks/`, owner-stamped, atomically written via tmp+rename, under the project mutex (see Concurrency contract).

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
| Trailing slash | Stripped from non-glob paths. `internal/store/` and `internal/store` are the same target. |
| Repo escape | Targets resolving outside the repo (`../../etc/passwd`) are rejected, exit 2. |
| Symlinks | Not resolved. The literal path is what gets locked. Two symlinked names for the same file are two distinct targets — the user is responsible for picking one. (We never touch the on-disk content; we only coordinate intent.) |
| Case sensitivity | Targets are case-sensitive. `Internal/Store` and `internal/store` are distinct. (Matches Go's filesystem assumptions and macOS HFS+/APFS default behavior.) |
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

loto doctor [--repair] [--dry-run] [--migrate-v1]
```

`refresh` is **not** a separate verb. Re-issuing `lock` against a target you already own refreshes intent and TTL atomically; this is the only way to extend a held lock. The verb count stays small.

**Read-only commands never mutate state.** `status`, `inbox`, `check-paths`, `whoami`, and bare `doctor` are pure reads. They surface stale or dead-pid locks in their output but do not reclaim. Reclamation belongs to `lock`, `break`, and `doctor --repair`.

`unlock --session` releases every lock with a `session_uuid` matching this session. Used by the SessionEnd hook so cleanup is precisely scoped to the dying session, not to every lock the agent's uuid has ever placed. `unlock --all-mine` is the broader manual escape — useful when an agent identity persists across multiple shells. `--session` is preferred for automation.

`status <target>` is the diagnostic command — the answer to "why can't I touch this?". Output includes: the exact lock at this target if any (live, stale, dead-pid); overlapping locks owned by others; tags on this target (own, addressed-to-me, system-authored); tags on overlapping targets relevant to me. It is the one command an agent runs when blocked.

Both `status` and `doctor` print a project identity header so a user reading the output across worktrees can orient quickly:

```
project: dkoosis-loto
repo:    /Users/dk/Projects/loto
state:   ~/.local/state/loto/projects/dkoosis-loto
```

Bare `loto doctor` with no flags is a read-only audit: prints stale locks, dead-pid holders, orphaned files, layout drift. `--repair` applies safe fixes; `--dry-run` shows what `--repair` would do without touching disk; `--migrate-v1` runs the one-shot v1→v2 on-disk conversion described in the migration section.

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
    ├── project.lock                               # project mutex; held during lock-set mutation
    ├── locks/<sha256(canonical-target)>.json      # one per locked target
    ├── tags/<sha256(canonical-target)>.jsonl      # append-only tag log per target
    └── tags/<sha256(canonical-target)>.lock       # per-target advisory file lock (POSIX flock)

~/.loto/agents/
    ├── <uuid>.json                                # host-global session identity record
    └── <uuid>/read-cursor.json                    # per-target last-read tag id (this agent)
```

**Project slug derivation.** Slug = `<owner>-<repo>` from the first `git remote get-url origin` host-path; falls back to `local-<sha256(repo-toplevel-abspath)[:8]>` when no `origin` remote exists. Multiple remotes use `origin` regardless of order. Detached worktrees and submodules resolve via `git rev-parse --show-toplevel` of the cwd. The exact derivation must match v1's existing implementation (see `loto.go:projectSlug`); the v2 spec adopts v1's behavior verbatim.

### Schemas

**Lock** (`locks/<sha256(canonical-target)>.json`):

```json
{
  "owner_uuid": "9e3c1e54-...",
  "session_uuid": "f70a3b22-...",
  "target": "internal/store/store.go",
  "intent": "store refactor — beads loto-7wp.4",
  "created_at": "2026-05-10T07:14:11Z",
  "expires_at": "2026-05-10T07:44:11Z",
  "host": "dk-mac",
  "pid": 84231,
  "branch": "store-refactor"
}
```

`session_uuid` is set at SessionStart by the same hook that exports `LOTO_AGENT_ID`. With the current identity model (one Claude session = new uuid per SessionStart), `session_uuid` is effectively redundant with `owner_uuid` — but recording it explicitly future-proofs the SessionEnd hook against any later change to identity persistence. `branch` is **display-only**: shown in holder reports for orientation, never used for overlap, takeover, or any safety decision.

**Tag** (one JSONL line in `tags/<sha256(canonical-target)>.jsonl`):

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

## Concurrency contract

The contract — what callers can rely on. Mechanics in the Implementation Notes appendix.

1. **Lock-set mutation is serialized under a project mutex.** A POSIX advisory lock on `$XDG_STATE_HOME/loto/projects/<slug>/project.lock` is held for the full critical section of every operation that mutates the lock set: `lock`, `unlock`, `break`, `doctor --repair`, `doctor --migrate-v1`. Without this, two concurrent `lock` invocations on overlapping-but-distinct targets can both pass the overlap check on stale reads and both write — the central safety claim becomes false.
2. **Lock files are written atomically.** `tmp + fsync + rename`. Readers either see the prior version or the new version, never a half-written file.
3. **Tags are append-only under a per-target tag lock.** A per-target POSIX advisory lock serializes appends and compaction. Removes the PIPE_BUF (4 KiB) constraint on append atomicity; large intent strings are safe.
4. **Read-only commands take no locks and mutate no state.** `status`, `inbox`, `check-paths`, `whoami`, bare `doctor` are pure reads. They surface stale state in their output but never reclaim.
5. **Break-force ordering is tag-first.** The system-authored break tag is appended (under the tag lock) **before** the lock file is replaced (under the project mutex). The two writes are not transactional, but the order guarantees that any observable state with the lock missing has the explanatory tag present.
6. **Refresh verifies ownership.** Re-issuing `lock` against an owned target reads the existing lock file under the project mutex, verifies `owner_uuid == me`, then writes the new lock file. A refresh cannot resurrect a lock that was just broken.
7. **Overlap detection correctness is release-blocking.** v1's `patternsOverlap` is the basis. v2 ships only after an exhaustive grammar test matrix covering identical patterns, prefix-containment, glob-vs-glob, glob-vs-literal, dot-segments, and brace expansion if supported. Any documented false-negative blocks release — false-negative means silent overlap-collision, which violates the central lock contract. False-positives (rejecting safe overlap) are tolerable.

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
| `break --force --reason` permits live takeover | **binary** | system-authored tag (`kind: system`, `event: lock_broken`, truthful `author_uuid`/`previous_owner_uuid`) is appended **before** the lock is replaced; `--reason` is required for both forms |
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

## Acceptance

A fresh Claude dropped into any worktree of a project where four other Claudes are working can:

1. Run `loto status` and understand who's locked what in <1s, with a project-identity header so cross-worktree orientation is immediate.
2. `loto lock <path>` with confidence that overlap is a hard refusal, not a silent overwrite — including under concurrent invocation by another agent on an overlapping target.
3. Receive a useful holder report when blocked — including held-since, TTL, intent, host, breaker's branch (display only).
4. Receive any addressed tags / want-next signals automatically on every `lock` and `unlock`, without remembering to check.
5. Tag a held file with "ping me when you're free" and trust the holder will see it on their next `unlock`.
6. Crash, restart, and resume — stale locks reclaim on the next `lock`/`break`/`doctor --repair`; no human cleanup, and `status` never silently mutates.
7. Have edits to another agent's locked paths refused at `PreToolUse`, before any disk write happens.
8. Two agents cannot concurrently acquire overlapping locks, even when `loto lock` is invoked at the same moment on different targets (project mutex serializes the critical section).
9. Equivalent path spellings normalize to the same target before lock, unlock, status, check-paths, and tag lookup.
10. Read-only commands (`status`, `inbox`, `check-paths`, `whoami`, bare `doctor`) never mutate lock or tag state.
11. `loto status <target>` makes it obvious why a target is clear, blocked, stale, or socially annotated.
12. `loto status --mine` and `loto status --session` make it obvious what cleanup will release.
13. A live lock break is visibly exceptional (`--force` required) and leaves a truthful system-authored audit tag (`kind: system`, `author_uuid` = breaker) on the target.
14. SessionEnd cleanup releases only locks created by the dying session, not every lock owned by the same agent uuid.

Any **write-safety invariant** described in this spec that the binary or hook does not enforce by exit code is a bug. Anything the binary or hook enforces that this spec doesn't describe is a misalignment to fix in one direction or the other. Tags are informational and intentionally do not enforce.

## Implementation notes

Mechanics that support the Concurrency contract; not part of the contract itself but load-bearing for any implementer.

### Lock files

- `tmp + fsync + rename` is the atomic write primitive. The replace is atomic at the directory entry level on POSIX local filesystems.
- The project mutex (`project.lock`) is acquired at the start of any lock-set mutation and released at the end. POSIX flock is process-bound, which matches our single-host single-fs assumption.
- Refresh under the project mutex: open existing lock JSON → verify `owner_uuid` → write new tmp → fsync → rename. Concurrent `break --force` either waits for the mutex or wins it first; either way, a refresh never resurrects a broken lock.

### Tag files

- Per-target `tags/<hash>.lock` (POSIX flock, exclusive) serializes append and compaction.
- Append: `O_APPEND` write of one JSONL line, with the per-target lock held. The lock removes the POSIX 4 KiB PIPE_BUF atomicity ceiling — long intent strings are safe.
- Compaction: triggered when the tag log exceeds 256 lines or 64 KiB. Holds the per-target lock, drops expired tags, rewrites via `tmp + fsync + rename`. A concurrent appender blocks until compaction completes, then appends to the rewritten file. No append is lost.
- Read: readers do not take the lock. They tolerate compaction's rename via stat-then-reopen on `ENOENT`. JSONL parsing is line-by-line; a partial last line is dropped (under the lock invariant, partial lines occur only mid-append, and the next read sees the complete line).

### Read cursor

- `~/.loto/agents/<uuid>/read-cursor.json` is rewritten via `tmp + rename` on `--mark-read`. Concurrent processes for the same agent are unexpected (one session = one process running CLI commands serially); if it ever happens, last-write-wins on the cursor is acceptable — losing a cursor advance just means re-seeing already-read tags, never missing a tag.

### Hook script details

The Stop hook (see Behavioral enforcement) emits JSON with `additionalContext` per the Claude Code 1.x hook protocol. The PreToolUse hook wrapper translates the binary's exit 1 (lock conflict) into the hook protocol's exit 2 (block tool call). Neither hook bends the binary's exit-code table; mapping lives in the wrapper.

### Patterns overlap test matrix

The release-blocking test matrix (Concurrency contract item 7) lives at `reservation_test.go` (or a successor module). At minimum it must cover: identical patterns; prefix containment in both directions; `**`-vs-literal; `**`-vs-`**`; literal-vs-literal disjoint; literal-vs-glob with shared literal prefix; brace expansion if our doublestar dependency supports it; dot-segment edge cases; trailing-slash variants. The bar is **zero documented false-negatives**; any false-negative blocks v2 release.
