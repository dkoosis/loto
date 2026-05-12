<!-- auto-published from KG (nug:0b105e61f67f) — do not edit -->
<!-- Source: /Users/vcto/Projects/dk/Project/dk/loto/docs/loto north star.md -->
<!-- Edits to this file are silently overwritten on next reconcile. Edit the source nug instead. -->

# loto north star

*Author: Claude. Audience: future Claudes (and dk).*
*Updated: 2026-05-11 — post-cut model (loto-vra): files-only, no mailbox, no glob.*

## what this is for

loto brings lockout/tagout to files. An agent locks a file while editing
it, so no other agent can change it at the same time.

The shape of the problem: five Claude Code sessions in the same repo,
each spawning subagents. All editing files. Without coordination they
clobber each other or panic on unexpected diffs. loto exists so any
Claude can answer one question fast:

> "Is it safe for me to edit this path right now, and if not, who's on it?"

If the answer arrives in <50ms, with structured JSON, with a *useful*
holder description, Claudes will actually use it. If it requires a daemon,
a network call, or human-readable-only output, they won't.

## non-goals

✗ Multi-host coordination (NFS, network shares — flock semantics break).
✗ A daemon. Claude can't reliably manage long-lived processes across sessions.
✗ Strong consistency. Tags are advisory; flock is the ground truth.
✗ Solving git conflicts. loto reduces them; git resolves them.
✗ Replacing review, tests, or human judgment. Coordination ≠ correctness.
✗ A chat system. loto coordinates files; conversation lives elsewhere.
✗ Enforcement. loto is advisory — a non-participating writer (vim, `sudo tee`, `chmod +w`) succeeds. Trust model = trust the operator.

## the model

```
$XDG_STATE_HOME/loto/                     # canonical, shared across subtrees
└── projects/<project-slug>/              # one per logical project (git remote-derived)
    ├── loto.db                           # SQLite: locks
    └── lock-op.flock                     # short-lived DB op serializer

~/.loto/agents/<uuid>.json                # host-global, session-persistent identities
```

SQLite tables:
- `locks` — one row per held target. Keyed by `target_canonical`. Carries owner, session, intent, expiry, host, pid, branch.

‡ **Identity is host-global, state is project-scoped.** Agent identity
lives at `~/.loto/agents/`, not under any project — one Claude session
touches many projects, and `LOTO_AGENT_ID` is exported once at SessionStart.

‡ **Single canonical base, project-scoped.** Without this, Claudes in
sibling worktrees of the same repo can't see each other. With it, they
coordinate transparently — no per-tree config, no `--base` argument in the
common case. Sidecar paths (`lock-op.flock`, future per-target sidecars)
hash the **canonical relative path** via `domain.Canonicalize`; the
project-scoped state dir disambiguates across projects.

‡ **`lock-op.flock` is a short-lived DB serializer, not a work lock.**
It is held only for the duration of an `acquire` or `release` operation
— milliseconds — so two overlapping `loto lock` invocations don't race on
the SQLite write. It is *never* held across user work, never held by an
editor, never visible in a blocker report. Work-hold semantics live in
the `locks` row + chmod, not in this flock. Forestalls the recurring
misread that op-flock is a foreground hold.

‡ **Coordination layers**, weakest to strongest. Shipped today are the
tag and enforcement layers; foreground file-flock and global flock remain
on the roadmap.

| Layer | Mechanism | Truth source | Status | Use case |
|------|-----------|--------------|--------|----------|
| Tag (record-tier) | `locks` row with non-zero, unexpired `expires_at` | row + TTL (lazy GC) | **shipped (v2)** | "I'm holding this across two events (PreToolUse → PostToolUse) — no foreground process" |
| **Enforcement (chmod)** | strip-write on each target on acquire; restore on release | filesystem mode bits | **shipped** | defeats naive writers + editors that honor perms; bypassable by `chmod +w` / `sudo` |
| Op-flock (internal) | project-wide flock on `lock-op.flock`, held only during an op | flock | **shipped** | serializes overlapping `loto lock` / `loto unlock` invocations |
| File flock (foreground) | flock(2) exclusive held by the editing process | flock | **deferred** (`loto with <cmd>`) | "I am editing this specific file right now" |
| Global lock | flock(2) on a project-wide handle | flock | **deferred** | "Sweep across the whole tree; everyone else stand down" |

‡ **Truth, not tags — with one bounded exception.** SQL rows can lie
(writer crashed mid-tx, row rotted past TTL). flock and filesystem mode
bits cannot. Every protocol decision involving a *foreground* operation
must remain valid if every `locks` row on disk is wrong or missing.
**Exception:** rows carrying a non-zero, unexpired `expires_at` are
authoritative for that TTL window — the record-tier carve-out, because
SQL state must persist across two CC hook events that flock (process-bound)
cannot bridge. TTL is the staleness guard: no daemon, no sweep, just
lazy GC on next acquire.

## liveness (no heartbeat)

A held lock is live if (a) its `expires_at` is in the future, and (b) if
its `host` matches this host, its `pid` is still alive. That's it. There
is no heartbeat — heartbeats imply a daemon, which we don't have. PID
probes are skipped for off-host rows (we can't observe them; TTL is the
only signal). Reclamation happens in three layers:

1. **Lazy GC on acquire** — every `loto lock` sweeps expired rows before
   evaluating its own request, chmod-restoring as it deletes.
2. **`loto doctor --repair`** — manual sweep for stale-but-still-held
   inconsistencies, orphaned `.lock`/`.tag` files, layout drift.
3. **SessionEnd hook** — eager `loto release --all-mine` on session exit.

When reclamation displaces a holder (forced break, GC of an expired
row), loto writes a `system` event so `loto status` and `loto doctor`
surface it. No silent dispossession.

## the operating loop (Claude's POV)

```
1. orient    → loto whoami                  # who am I in this session?
2. acquire   → loto lock <file>...          # multi-file atomic; exit 0, or exit 1 + blocker rows
3. edit      → ... do the work ...
4. release   → loto unlock <file>...        # per-target best-effort, or auto on session end
```

Multi-file lock is all-or-nothing: any blocker aborts the set, no chmod
side effects, no rows inserted. Unlock is per-target best-effort
(missing and not-owner are distinct outcomes — gh#46).

Every command emits JSON when stdout is not a tty (or when `--json`).
Exit codes are stable: `0` success, `1` advisory conflict, `2` usage,
`3` IO/system. Holder identity always rides on the error.

## what makes this Claude-friendly

**Identity that survives `exec`.** Each Claude session gets one handle —
adjective+noun, PascalCase, GitHub-style: `GreenCastle`, `BlueLake`. A
SessionStart hook writes `~/.loto/agents/<uuid>.json` and exports
`LOTO_AGENT_ID`. Every shell-out from that session inherits the env, so
locks taken by `bash -c "loto lock ..."` and locks taken by a subagent
worktree are owned by the same identity. This is the keystone — without
it, "release my locks on session end" is meaningless.

**Useful holder reports.** When a Claude is blocked, it should not see
`flock: EWOULDBLOCK`. It should see:

```json
{
  "blocked_by": "GreenCastle",
  "intent": "refactor store package — see beads loto-7wp.4",
  "kind": "file",
  "target": "/Users/dk/Projects/foo/internal/store/store.go",
  "held_since": "2026-04-28T14:32:11Z",
  "expires_at": "2026-04-28T14:42:11Z",
  "branch": "store-refactor",
  "host": "dk-mac",
  "pid": 84231
}
```

The blocked Claude can then decide: wait or work elsewhere. Both are one
command away.

**Soft-TTL on rows.** A `locks` row carries `expires_at`. Past expiry
it's *soft-stale*: still present, flagged in status output, eligible for
GC on next acquirer's pass. Lets a Claude declare "I'll touch this within
10min" without holding a process open the whole time. For the file-flock
tier (deferred), flock will remain authoritative for *currently* held;
TTL just bounds *advisory* signals on the record tier.

**Filesystem enforcement on lock.** Acquiring a lock strips owner-write
bits (`mode &^ 0222`); releasing restores owner-write (`mode | 0200`).
Group/other-write bits are not preserved across a lock cycle — lossy by
design, no `original_mode` column, no migration. Defeats naive writers
and editors that honor perms; trivially bypassable by `chmod +w`. That's
fine: trust model = trust the operator.

**Pre-commit hook as the safety net.** `loto install-hook` writes a git
pre-commit that runs `loto check-paths --staged` and refuses the commit
if any staged path is held by *another* agent's exclusive lock. This is
the moment that matters: not the edit, the commit. `--no-verify` remains
the user's escape hatch — bypass is unobservable to loto by definition
(the hook didn't run), and that's fine. Trust model = trust the operator.

**`loto doctor`.** One command for diagnostics: dead-PID holders,
orphaned `.lock` files, layout drift, soft-stale-but-still-held
inconsistencies. `loto doctor --repair` applies safe fixes; `--dry-run`
previews. This is what a Claude runs when something feels off, instead of
poking around the filesystem.

**Composable, not monolithic.** loto pairs with siblings — it doesn't
absorb them.

```bash
# next + loto, the unix way
path=$(next claim --treatment=lint)
loto lock "$path" --json && {
  # ... do the lint work ...
  loto unlock "$path"
  next done --path "$path" --result "$(git rev-parse HEAD)"
}
```

If we later add a `loto with-next` wrapper, it's sugar — the primitives
stay separable. Same posture toward beads, snipe, etc.

## what 5 concurrent Claudes look like

Imagine: BlueOak, GreenCastle, RedRiver, AmberFox, SilverPine all open in
the same project. Each has a worktree under `~/Projects/foo-wt-<handle>/`.

```
project-state ($XDG_STATE_HOME/loto/projects/foo/loto.db):

  locks (held):
    internal/store/store.go    ← BlueOak     (held 4m, expires 6m, mode 0444)
    cmd/foo/main.go            ← RedRiver    (held 30s, expires 9m30s, mode 0444)

  agents (active):
    BlueOak       last_seen: 12s ago    branch: store-refactor
    GreenCastle   last_seen: 2m ago     branch: docs-pass
    RedRiver      last_seen: 8s ago     branch: cli-flag-cleanup
    AmberFox      last_seen: 45s ago    branch: <none — exploring>
    SilverPine    last_seen: 11s ago    branch: bug-loto-7wp.5
```

When AmberFox reads `internal/store/store.go`, no lock needed — the
file is `0444`, still readable. loto coordinates writes only. When
AmberFox tries to *edit* it: `loto lock internal/store/store.go` returns
blocker JSON showing BlueOak holds it for ~6 more minutes with a clear
intent. AmberFox's Claude sees that and picks different work.

When dk's Claude session ends (or crashes), the SessionEnd hook runs
`loto release --all-mine`, which uses `LOTO_AGENT_ID` to find and release
that session's holdings. The next agent's lazy GC catches anything
missed; `loto doctor --repair` mops up the rest.

## design invariants (load-bearing)

1. **flock + filesystem are truth, with one bounded exception.** Every
   protocol decision involving a *foreground* hold must remain valid if
   every `locks` row on disk is wrong or missing. (✗ never trust a SQL
   row for the safety of a foreground operation; only for description.)
   **Exception:** rows carrying a non-zero, unexpired `expires_at` are
   authoritative for that TTL window — the record-tier carve-out, because
   SQL state must persist across two CC hook events that flock
   (process-bound) cannot bridge. TTL governs record-tier holds; flock
   governs (future) foreground holds.
2. **Single host.** Canonical paths on this machine. ✗ NFS, ✗ remote.
3. **No daemon.** Every operation is a fresh process. State lives on disk.
4. **JSON-first I/O.** Human formatting is opt-in. Exit codes are stable
   (`0` success, `1` advisory conflict, `2` usage, `3` IO/system).
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes. ✗ never gate reads.
   chmod enforcement strips write only — files remain readable at `0444`.
7. **Cleanup is layered.** SessionEnd hook (eager) + lazy GC on next
   acquire (passive) + `loto doctor --repair` (manual). Each layer assumes
   the others may fail. Lazy GC chmod-restores stale rows before deletion.
8. **No silent dispossession — of locks or of bytes.** Any forced release
   writes a system event observable via `loto status` / `loto doctor`.
   Orphan-mode files (stripped on disk, no DB row) are flagged by
   `doctor` but never restored without explicit `--restore-orphan-mode`.

## what we are *not* building

- Not a chat system. Use Slack for conversation; loto coordinates files.
- Not a workflow engine. loto says who's on what; beads says what's next;
  next says what files; snipe says where in the code. They compose; loto
  doesn't merge them.
- Not a permissions system. Any agent can break any stale lock. Trust
  model = trust the operator.
- Not a general transaction system. We ship multi-file atomic acquire
  for one case — cooperating Claudes mid-sweep need the changed file set
  to land or not-land together, not race per-file. No multi-target
  *unlock* atomicity (release is best-effort per target).

## smell tests

If you find yourself writing one of these, stop and reconsider:

- A new daemon, listener, or background process
- A protocol that requires *both* agents to be running
- A code path that trusts a `locks` row for a *correctness* decision **outside the record-tier carve-out** (foreground holds: flock is truth; record-tier acquires: TTL is truth — anything else, stop)
- A schema migration tool for the on-disk layout (we wipe on `user_version` mismatch via `MoveCorruptAside` — three lines, no NULL-tolerance complexity)
- A `--unsafe-disable-flock` flag
- An `original_mode` column to round-trip lock cycles losslessly (rejected by chmod-policy: lossy `mode | 0200` restore is the chosen trade)
- A heartbeat, keepalive, or any background liveness pinger

If a feature can't be expressed as "what does the next single `loto`
invocation do, given the current state of `$LOTO_HOME`?", it's probably
in the wrong layer.

## end-state acceptance

We reach the north star when a fresh Claude, dropped into any worktree
of a project where 4 other Claudes are working, can:

1. Run `loto status --json` and understand who's on what in <1s.
2. Acquire one or more file locks atomically, and edit safely.
3. Receive a useful blocker report when something is held.
4. Crash, restart, and resume without leaving stale state — including filesystem-mode state.
5. Commit through a hook that catches the one mistake humans make.

That's the bar. Everything in the backlog (loto-7wp.*) is a step toward
it. Anything else is scope creep.