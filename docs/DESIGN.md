<!-- auto-published from KG (nug:3aa4cdc415eb) — edit source nug, not this file -->

# loto design

*Author: dk. Audience: future Claudes (and dk). The spec behind the charter (`docs/NORTH_STAR.md`); the elevator pitch is `docs/NORTH_STAR_MINI.md`.*

## the model

```
$XDG_STATE_HOME/loto/                     # canonical, shared across subtrees
└── projects/<project-slug>/              # one per logical project (git remote-derived)
    ├── loto.db                           # SQLite: locks
    └── lock-op.flock                     # short-lived DB op serializer

~/.loto/agents/<uuid>.json                # host-global, session-persistent identities
```

SQLite tables:
- `locks` — one row per holder per target. Keyed by the composite PK `(target_canonical, owner_uuid)` so a target can carry several coexisting shared holders. Carries owner, session, intent, expiry, host, pid, branch, **mode** (`shared` | `exclusive`; empty legacy → exclusive).

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
the `locks` row, not in this flock. Forestalls the recurring
misread that op-flock is a foreground hold.

‡ **Coordination layers**, weakest to strongest. Shipped today are the
tag and enforcement layers; foreground file-flock and global flock remain
on the roadmap.

| Layer | Mechanism | Truth source | Status | Use case |
|------|-----------|--------------|--------|----------|
| Tag (record-tier) | `locks` row with non-zero, unexpired `expires_at` | row + TTL (lazy GC) | **shipped (v2)** | "I'm holding this across two events (PreToolUse → PostToolUse) — no foreground process" |
| **Enforcement (chmod)** | strip-write on acquire; restore on release | filesystem mode bits | **retired** (loto-zssw) | locked out the HOLDER too — see "why chmod enforcement is gone" |
| Harness gate | PreToolUse hook refuses a write over a peer's live lock/claim/beacon | `locks` + `claims` rows | **shipped** | stops the peer without touching the holder's own filesystem |
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
   evaluating its own request.
2. **`loto doctor --repair`** — manual sweep for stale-but-still-held
   inconsistencies, orphaned `.lock`/`.tag` files, layout drift.
3. **SessionEnd hook** — eager `loto release --all-mine` on session exit.

When reclamation displaces a holder (forced break, GC of an expired
row), loto writes a `system` event so `loto status` and `loto doctor`
surface it. No silent dispossession.

## lock modes (shared / exclusive)

A lock is `exclusive` (sole writer — the default) or `shared` (multi-reader
lease). The point: several agents can declare a read-hold on the same target
without false contention, and a writer can step down to a reader in place when
a peer needs to read alongside — "conflicts as a negotiation, not a wall."

```
LockMode = shared | exclusive          -- empty/legacy reads as exclusive

Conflicts(a, l):                        -- incoming a vs existing holder l
  same owner            → false         -- re-acquire is an upsert
  different target       → false
  l is stale            → false         -- reclaimable; never a hard block
  else → a.exclusive OR l.exclusive     -- shared+shared coexist; exclusive walls
```

Invariants:
- **I1** shared + shared on one target coexist (any number of readers).
- **I2** exclusive on either side conflicts (sole writer).
- **I3** no lock of any mode touches file permissions (loto-zssw). The mode
  field governs conflict resolution and nothing on disk.
- **I4** `downgrade` flips exclusive → shared in place — no unlock/relock, no new
  `created_at`, the hold is continuous. Already-shared is a no-op. There is **no** shared → exclusive upgrade (a non-goal).
- **I5** under the composite PK, a per-owner lookup uses `LockForOwnerAt`; a
  release/downgrade always resolves the caller's **own** row, never a peer's.
- **I6** `check --staged` is **liveness-gated**: a *provably-live* exclusive peer
  is a hard block (exit 1); an indeterminate/expiring exclusive peer (PID-0
  sentinel, cross-host) is an advisory `✓ … liveness=unknown` that does not block;
  a shared peer never blocks. (Whether to weaken further to pure grant-with-warning
  remains an open dk decision.)

CLI: `loto lock <t> --shared` takes a read lease (default is exclusive);
`loto downgrade <t>` steps an exclusive hold down to shared;
`loto refresh <t> [--ttl d] | --all` extends the lease you already hold.

## the operating loop (Claude's POV)

```
1. orient    → loto whoami                  # who am I in this session?
2. acquire   → loto lock <file>...          # multi-file atomic; exit 0, or exit 1 + blocker rows
3. edit      → ... do the work ...
4. release   → loto unlock <file>...        # per-target best-effort, or auto on session end
```

Multi-file lock is all-or-nothing: any blocker aborts the set, no rows
inserted. Unlock is per-target best-effort
(missing and not-owner are distinct outcomes — gh#46).

Output is Claude-optimized KV: deterministic order, one record per line,
fixed glyphs per `.claude/rules/design.md`. Exit codes are stable:
`0` success, `1` advisory conflict, `2` usage, `3` IO/system. Holder
identity always rides on the error.

## what makes this Claude-friendly

**Identity that survives `exec`.** Each Claude session gets one handle —
adjective+noun, PascalCase, GitHub-style: `GreenCastle`, `BlueLake`. A
SessionStart hook writes `~/.loto/agents/<uuid>.json` and exports
`LOTO_AGENT_ID`. Every shell-out from that session inherits the env, so
locks taken by `bash -c "loto lock ..."` and locks taken by a subagent
worktree are owned by the same identity. This is the keystone — without
it, "release my locks on session end" is meaningless.

**Useful holder reports.** When a Claude is blocked, it sees a KV row
with everything it needs to decide: handle, intent, target, held_since,
expires_at, branch, host, pid. The blocked Claude can then decide: wait
or work elsewhere. Both are one command away.

**One liveness oracle.** "Is the session behind this agent still up?" is
answered once, in `identity.AgentLive` (loto-ygty): messaging-socket
existence, then a start-time identity check on the socket's pid — the OS
start-time recorded at peer-record time must still match the pid's current
start-time, so a recycled pid reads DEAD (loto-uxhg). Records with no
recorded start-time (written before the field existed, or on a platform
with no reader) fall back to matching `--agent-id` / `--parent-session-id`
against the occupant's argv — which closes PID reuse for subagents only,
since a top-level session carries neither flag. Verdicts: LIVE (socket +
start-time or ps check out — overrides the lock-row pid probe, so a
worktree agent whose stamped pid probes dead is not misread, loto-r11w),
DEAD (socket gone, pid gone, start-time mismatch, or argv mismatch —
fast-reclaim), UNKNOWN (no peer record — TTL is the sole authority; never
treated as dead). `loto locks`' staleness, `loto who`'s pruning, and `loto
alive` (the verb kill-class tooling like the bdx reaper calls) all consume
this one check. A DEAD verdict is necessary but never sufficient for
killing an agent: kill-class actions also
require an idle/TaskStop signal from the harness.

**Soft-TTL on rows.** A `locks` row carries `expires_at`. Past expiry
it's *soft-stale*: still present, flagged in status output, eligible for
GC on next acquirer's pass. Lets a Claude declare "I'll touch this within
30min" without holding a process open the whole time. A crashed holder
frees its territory at the deadline with no `doctor` run — doctor stays
for forensics and orphan-mode repair, not for reclamation. A holder still
alive proves it with `loto refresh`, which extends `expires_at` in place
(same row, same `created_at`); refreshing an already-lapsed lease is
refused, since a peer may already be taking it. Naming a lapsed target
explicitly is a `✗` and exit 1; the same row under `--all` is a `⚠`
advisory and exit 0, so one stale lock can't fail a heartbeat that
extended every live lease it owns. For the file-flock
tier (deferred), flock will remain authoritative for *currently* held;
TTL just bounds *advisory* signals on the record tier.

**Why chmod enforcement is gone (loto-zssw).** Until 2026-08, acquiring a
lock stripped owner-write (`mode &^ 0222`) and releasing restored it
(`mode | 0200`). The holder edits through the same filesystem as everyone
else, so the strip locked out the agent whose whole operating loop is
lock → edit → unlock. On 2026-08-12 a holder's own `>` redirect failed
silently inside a `&&` chain and a stale `NORTH_STAR.md` body reached
main; files were also observed orphaned at `0444` with no row to explain
them after a session died mid-hold.

Peer protection now lives entirely at the harness gate, one layer up,
where refusing a peer costs the holder nothing. `loto doctor --repair`
carries a one-way migration that re-adds owner-write to chmod-era
stragglers; its candidate set is the current lock rows plus the retained
audit trail, so it drains itself as events rotate.

**Pre-commit hook (deferred).** A pre-commit `loto check --staged` that
refuses commits over another agent's held paths was prototyped in
loto-ux3 and cut in the v2 entrypoint switch (commit 3d5f3de). The
record tier plus the harness gate already catch most edits before they
land; revisit if mistakes cluster at commit time.

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
loto lock "$path" -t "lint sweep" && {
  # ... do the lint work ...
  loto unlock "$path" -t "lint done"
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
    internal/store/store.go    ← BlueOak     (held 4m, expires 26m, exclusive)
    cmd/foo/main.go            ← RedRiver    (held 30s, expires 29m30s, exclusive)

  agents (active):
    BlueOak       last_seen: 12s ago    branch: store-refactor
    GreenCastle   last_seen: 2m ago     branch: docs-pass
    RedRiver      last_seen: 8s ago     branch: cli-flag-cleanup
    AmberFox      last_seen: 45s ago    branch: <none — exploring>
    SilverPine    last_seen: 11s ago    branch: bug-loto-7wp.5
```

When AmberFox reads `internal/store/store.go`, no lock needed — a lock
never gates a read, and never changes the file. loto coordinates writes
only. When
AmberFox tries to *edit* it: `loto lock internal/store/store.go` returns
a blocker row showing BlueOak holds it for ~26 more minutes with a clear
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
4. **Claude-optimized KV output.** Deterministic order, fixed glyphs per
   `.claude/rules/design.md`. Exit codes stable (`0` success, `1` advisory
   conflict, `2` usage, `3` IO/system).
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes. ✗ never gate reads.
   A lock is a row, not a mode change — the file on disk is untouched.
7. **Cleanup is layered.** SessionEnd hook (eager) + lazy GC on next
   acquire (passive) + `loto doctor --repair` (manual). Each layer assumes
   the others may fail.
8. **No silent dispossession — of locks or of bytes.** Any forced release
   writes a system event observable via `loto status` / `loto doctor`.
   Chmod-era residue (read-only on disk, no DB row) is flagged by `doctor`
   but never restored without explicit `--restore-orphan-mode`, since a
   read-only file with no row may simply be one the user meant to protect.
9. **A clean verdict is a claim, not a default.** The gate may decline to
   answer (fail-open, exit 3, `⚠` to stderr); it may never answer `✓ no
   conflicts` on a path it could not resolve. A false clean is strictly
   worse than no check, because it reads as protection (loto-tzmv.10).

### path resolution differs per calling tool

The base a relative path resolves against is a property of the *caller*,
not of loto, and the two shell call sites do not agree:

| caller | relative path | why |
|---|---|---|
| `Bash` | payload cwd + any leading `cd` in the same command | the process is fresh per call, so the payload's cwd is the shell's cwd |
| `mcp__trixi__agent_shell` | **unresolvable — refuse** | the shell keeps its own persistent cwd, set by a `cd` that may have run three tool calls ago and absent from the payload |

A caller that cannot vouch for the base passes `--cwd-unknown`; loto then
refuses relative tokens (`✗ unresolvable`) rather than guessing. Absolute
paths carry their own base and are unaffected. The declaration is the
caller's because only the caller knows its own tool shape — loto must not
sniff a tool name, or the rule drifts between the two call sites.

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

## tags (annotations bound to locks)

`loto tag <file> <text>` leaves a note on a target that's locked by another
agent. Tags are parasitic on the host lock identified by
(target_canonical, owner_uuid, lock_created_at): when that triple disappears
the tag is orphaned (read-time filter, GC'd by `doctor --repair`).

Invariants:

- **I1** external tag (`tagger ≠ lock_owner`) → counted for cap, in holder echo.
- **I2** self-tag (`tagger = lock_owner`) → counted for cap, **not** in holder echo.
- **I3** `Alive(t) ⟹ Host(t) ∃` — parasitic; no orphan can be alive.
- **I4** `acked_at` monotonic (set once, never unset).
- **I5** text is append-only — no UPDATE path.
- **I6** cap of 5 alive tags per (file, lock-instance), enforced inside the same tx as the INSERT (no TOCTOU).

Surfaces (the holder is forced to see):

| caller \ tag                | external | self-tag |
|-----------------------------|----------|----------|
| holder runs runtime cmd     | ✓ trailing footer | ✗ |
| holder runs `ack`           | ✓ (acks it)       | n/a |
| holder runs `unlock`        | ✓ implicit ack    | ✓ implicit ack |
| non-holder `status f`       | ✓ inline          | ✓ inline |
| non-holder `lock f` (fail)  | ✓ via EmitConflict| ✓ via EmitConflict |

`loto check` is excluded — its output is a pinned machine surface that
trixi's PreToolUse hook parses on every `.go` save. Holders pick up tags
via the next `lock` / `unlock` / `status` / `doctor` instead.

Lifecycle (matches `locks_release.go::ackTagsForReleaseTx` + `doctor.go::gcTagsTx`):

| event                      | effect on the host lock's tags        |
|----------------------------|---------------------------------------|
| `loto unlock` by holder    | UPDATE acked_at inside the release tx |
| `loto unlock --force` break| tags orphaned, no implicit ack        |
| lock expiry                | tags orphaned                         |
| holder re-acquires         | new `lock_created_at` → previous tags stay dead |
| `loto ack <id>` by holder  | one tag acked                         |
| `loto doctor --repair`     | hard-deletes orphans + acked > 7d     |

‡ Schema v6 → v7 means a one-time `MoveCorruptAside` on first open after
this change: the audit `events` log lives in the same DB and is lost too.
Acceptable trade — locks are ephemeral and events retention is already 7d.

## smell tests

If you find yourself writing one of these, stop and reconsider:

- A new daemon, listener, or background process
- A protocol that requires *both* agents to be running
- A code path that trusts a `locks` row for a *correctness* decision **outside the record-tier carve-out** (foreground holds: flock is truth; record-tier acquires: TTL is truth — anything else, stop)
- A schema migration tool for the on-disk layout (we wipe on `user_version` mismatch via `MoveCorruptAside` — three lines, no NULL-tolerance complexity)
- A `--unsafe-disable-flock` flag
- An `original_mode` column to round-trip lock cycles losslessly (moot since loto-zssw: loto changes no mode at all)
- A heartbeat, keepalive, or any background liveness pinger

If a feature can't be expressed as "what does the next single `loto`
invocation do, given the current state of `$LOTO_HOME`?", it's probably
in the wrong layer.
