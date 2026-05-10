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
- **TTL.** Every lock carries an expiry. The owner may `refresh` to extend. Past TTL the lock is *stale*: it still occupies the slot until reclaimed.
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

## CLI surface

```
loto whoami
loto lock <target> [--ttl 30m] [--intent "..."]
loto unlock <target>
loto refresh <target> [--ttl 30m]
loto break <target> --force --reason "..."

loto tag <target> [--to <agent>] [--ttl 1h] "<note>"
loto untag <target> <tag-id>

loto status [<target>...]
loto inbox [--mine] [--unread]
loto check-paths <target>...

loto doctor [--repair] [--dry-run]
```

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
| any | same target, same agent | idempotent refresh, exit 0 |
| any | overlapping target, same agent | exit 0, lock placed (a refinement, not a conflict — same agent can hold both `internal/store/**` and `internal/store/store.go`) |
| any | overlapping target, different agent | exit 1, holder report |
| any | non-overlapping target | exit 0, lock placed |

`loto lock '**'` is the equivalent of v1's global lock: it succeeds only if no other locks exist, since `**` overlaps every possible target.

Overlapping tags are always allowed (tags don't enforce); on `tag add`, the binary surfaces existing tags on the same or overlapping targets as a `⚠ overlaps existing` block in the response so the author sees who else is in the area.

## Persistence

```
$XDG_STATE_HOME/loto/                              # canonical, shared across worktrees
└── projects/<project-slug>/                       # one per logical project (git remote-derived)
    ├── locks/<sha256(target)>.json                # one per locked target
    └── tags/<sha256(target)>.jsonl                # append-only tag log per target

~/.loto/agents/<uuid>.json                         # host-global session identity
```

- **Locks**: one JSON file per locked target. Atomic write (tmp+rename). Body: `{owner, target, intent, created_at, expires_at, host, pid, branch}`.
- **Tags**: JSONL append-only per target. Each line is one tag with `{id, author, target, intent, addressee, created_at, expires_at}`. Append is atomic at line granularity on local filesystems.
- **Lazy GC**: stale locks (past TTL) and expired tags are pruned by any agent's next access to that target, and by `loto doctor --repair`. No daemon, no sweep.

## Behavioral enforcement

The binary enforces the on-disk contract — overlap rejection, owner-only release, single-target single-lock, no silent dispossession, atomic writes. Hooks enforce that Claude follows the workflow. Skills are documentation of last resort.

| Behavior to enforce | Where it lives | What it does |
|---|---|---|
| Release my locks on session end | `SessionEnd` hook | runs `loto unlock --all-mine` |
| Don't edit a path locked by another agent | `PreToolUse` on Edit/Write/MultiEdit | runs `loto check-paths` against the tool input; exit 2 with a holder report if any input path overlaps another agent's lock |
| Don't commit changes that touch another agent's locked paths | git `pre-commit` hook | runs `loto check-paths --staged`; refuses the commit on conflict |
| Surface addressed tags + want-next on lock acquire | **binary** | `loto lock` success output includes any tags on the target where `addressee == me` or where the tag is unaddressed-but-relevant (e.g. prior holder's heads-up) |
| Surface relevant tags on lock release | **binary** | `loto unlock` reads tags on the target before returning; if any have an addressee, prints a `→ notify` block listing each so the releasing agent sees who's waiting |
| Periodic inbox check during a long session | `Stop` hook + binary | hook runs `loto inbox --mine --unread`; if non-empty, injects unread tags into the next-prompt context. Binary already surfaces on every lock/unlock, so this catches sessions with no lock activity |
| `break --force` writes the dispossession tag | **binary** | atomic part of the break operation, not a separate step; `--reason` is required |
| Same-agent identical-target re-lock is a refresh | **binary** | idempotent — update intent/TTL in place |

The pre-edit and pre-commit hooks are the load-bearing safety net. They turn "remember to check before editing" (paper shield) into an enforced exit-2 refusal.

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
