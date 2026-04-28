# loto north star

*Author: Claude. Audience: future Claudes (and dk).*

## what this is for

Five Claude Code sessions, same repo, different subtrees, each spawning
subagents. All editing files. Today they clobber each other or panic on
unexpected diffs. loto exists so any Claude can answer one question fast:

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

## the model

```
$XDG_STATE_HOME/loto/                     # canonical, shared across subtrees
└── projects/<project-slug>/              # one per logical project (git remote-derived)
    ├── global.lock                       # flock'd shared/exclusive
    ├── global.tag                        # JSON: who holds global, why
    ├── files/<sha256(abs-path)>.lock     # one per target file
    ├── files/<sha256(abs-path)>.tag      # one per target file
    ├── files/<sha256(abs-path)>.msgs     # JSONL append-only mailbox
    ├── reservations/<sha256(glob)>.tag   # advisory pattern reservations
    └── agents/<handle>.json              # session-persistent identities
```

‡ **Single canonical base, project-scoped.** Without this, Claudes in
sibling worktrees of the same repo can't see each other. With it, they
coordinate transparently — no per-tree config, no `--base` argument in the
common case.

‡ **Three coordination tiers**, weakest to strongest:

| Tier | Mechanism | Truth source | Use case |
|------|-----------|--------------|----------|
| Reservation | tag at `reservations/<hash>.tag` matching a glob | tag presence | "I plan to refactor `internal/store/**` over the next hour" |
| File lock | flock(2) exclusive on `files/<hash>.lock` + tag | flock | "I am editing this specific file right now" |
| Global lock | flock(2) exclusive on `global.lock` + tag | flock | "Sweep across the whole tree; everyone else stand down" |

‡ **Tags are descriptive, flock is authoritative.** Tags can lie (writer
crashed mid-write, tag rotted past TTL). flock cannot — if you can acquire
it, no one holds it. Every protocol decision flows from this.

## the operating loop (Claude's POV)

```
1. orient    → loto whoami            # who am I in this session?
2. intend    → loto reserve <glob>    # optional: stake a soft claim
3. acquire   → loto try file <path>   # exit 0 + handle, or exit 1 + holder JSON
4. edit      → ... do the work ...
5. read msgs → loto inbox --since-acquire   # surface stuff aimed at me
6. release   → loto release <handle>  # explicit, or auto on session end
```

Every command emits JSON when stdout is not a tty (or when `--json`).
Exit codes are stable: `0` success, `1` advisory conflict, `2` usage,
`3` IO/system. Holder identity always rides on the error.

## what makes this Claude-friendly

**Identity that survives `exec`.** Each Claude session gets one handle —
adjective+noun, PascalCase, GitHub-style: `GreenCastle`, `BlueLake`. A
SessionStart hook writes `~/.loto/agents/<uuid>.json` and exports
`LOTO_AGENT_ID`. Every shell-out from that session inherits the env, so
locks taken by `bash -c "loto try ..."` and locks taken by a subagent
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

The blocked Claude can then decide: wait, work elsewhere, or message
GreenCastle ("can I get this in 5 min?"). All three are one command away.

**Mailbox piggybacked on the file.** When a Claude acquires a lock, it
reads any messages addressed to it (or `@all`) on that target since its
last acquire. Messages are JSONL appended to `<hash>.msgs` — no daemon, no
sockets, no schema migration. Use cases:

- "I broke the test on line 40 before releasing this — heads up."
- "Merging this into main in ~10min, hold off."
- "@all: I'm renaming `Foo` → `Bar` in this file; refresh your imports."

‡ When `loto break` or GC reclaims a stale lock, it appends a system
message to the displaced agent's mailbox describing what was broken and
why. No silent dispossession.

**Soft-TTL on tags.** A tag may carry `expires_at`. Past expiry it's
*soft-stale*: still present, but flagged in status output, eligible for GC
if its flock is also unheld. Lets a Claude declare "I'll touch this within
10min" without holding a process open the whole time. flock remains
authoritative for *currently* held; TTL just bounds *advisory* signals.

**Glob reservations as the middle tier.** A Claude doing feature work on
`internal/store/**` can stake one reservation, see other agents' file
acquires within that pattern surface as gentle conflicts, and avoid the
all-or-nothing choice between per-file and global. Reservations are
advisory only — they cause warnings at acquire, not blocks. Escalation
happens at the git pre-commit hook (next item).

**Pre-commit hook as the safety net.** `loto install-hook` writes a git
pre-commit that runs `loto check-paths --staged` and refuses the commit
if any staged path is held by *another* agent's exclusive lock or matches
their exclusive reservation. This is the moment that matters: not the
edit, the commit. `--no-verify` remains the user's escape hatch (and the
hook logs the bypass to the affected agents' mailboxes).

**`loto doctor`.** One command for diagnostics: stale tags, dead-PID
holders, orphaned `.lock`/`.tag` files, layout drift, soft-stale-but-still-held
inconsistencies. `loto doctor --repair` applies safe fixes; `--dry-run`
previews. This is what a Claude runs when something feels off, instead of
poking around the filesystem.

**Composable, not monolithic.** loto pairs with siblings — it doesn't
absorb them.

```bash
# next + loto, the unix way
path=$(next claim --treatment=lint)
loto try file "$path" --json | jq -e '.acquired' && {
  # ... do the lint work ...
  loto release --target "$path"
  next done --path "$path" --result "$(git rev-parse HEAD)"
}
```

If we later add a `loto with-next` wrapper, it's sugar — the primitives
stay separable. Same posture toward beads, snipe, etc.

## what 5 concurrent Claudes look like

Imagine: BlueOak, GreenCastle, RedRiver, AmberFox, SilverPine all open in
the same project. Each has a worktree under `~/Projects/foo-wt-<handle>/`.

```
project-state ($XDG_STATE_HOME/loto/projects/foo/):

  reservations:
    "internal/store/**"  ← BlueOak     (intent: store refactor, 1h TTL)
    "docs/**"            ← GreenCastle (intent: README rewrite, 30m TTL)

  file locks (held):
    internal/store/store.go    ← BlueOak     (held 4m, expires 6m)
    cmd/foo/main.go            ← RedRiver    (held 30s, expires 9m30s)

  agents (active):
    BlueOak       last_seen: 12s ago    branch: store-refactor
    GreenCastle   last_seen: 2m ago     branch: docs-pass
    RedRiver      last_seen: 8s ago     branch: cli-flag-cleanup
    AmberFox      last_seen: 45s ago    branch: <none — exploring>
    SilverPine    last_seen: 11s ago    branch: bug-loto-7wp.5
```

When AmberFox decides to read `internal/store/store.go`, no lock needed
(reads are unrestricted; loto coordinates writes). When AmberFox decides
to *edit* it: `loto try file internal/store/store.go` returns blocker JSON
showing BlueOak holds it for ~6 more minutes with a clear intent.
AmberFox's Claude sees that and either picks different work or sends:

```bash
loto msg internal/store/store.go --to BlueOak \
  "Need to add a 3-line method here for loto-7wp.11 — yield in ~2min ok?"
```

BlueOak's next loop iteration reads its inbox on lock-acquire, sees the
message, finishes, releases. AmberFox's polling acquire succeeds. No
human in the loop. No clobber. No panic.

When dk's Claude session ends (or crashes), the SessionEnd hook runs
`loto release --all-mine`, which uses `LOTO_AGENT_ID` to find and release
exactly that session's holdings. Anything missed is caught by the next
agent's lazy GC or by a periodic `loto doctor --repair`.

## design invariants (load-bearing)

1. **flock is truth.** Every protocol decision must remain valid if every
   tag on disk is wrong or missing. (✗ never read a tag and trust it for
   safety; only for description.)
2. **Single host.** All paths are absolute on this machine. ✗ NFS, ✗ remote.
3. **No daemon.** Every operation is a fresh process. State lives on disk.
4. **JSON-first I/O.** Human formatting is opt-in. Exit codes are stable.
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes. ✗ never gate reads.
7. **Cleanup is layered.** SessionEnd hook (eager) + lazy GC on next
   acquire (passive) + `loto doctor --repair` (manual). Each layer assumes
   the others may fail.
8. **No silent dispossession.** Any forced release notifies the displaced
   agent through their mailbox.

## what we are *not* building

- Not a chat system. Mailboxes are file-scoped, message-truncated,
  best-effort. Use Slack for conversation.
- Not a workflow engine. loto says who's on what; beads says what's next;
  next says what files; snipe says where in the code. They compose; loto
  doesn't merge them.
- Not a permissions system. Any agent can break any stale lock. Trust
  model = trust the operator.
- Not a transaction system. ✗ multi-file atomic acquire (yet — could be
  added if it ever proves needed; YAGNI for now).

## smell tests

If you find yourself writing one of these, stop and reconsider:

- A new daemon, listener, or background process
- A protocol that requires *both* agents to be running
- A code path that trusts a tag for a *correctness* decision
- A schema migration tool for the on-disk layout
- A `--unsafe-disable-flock` flag

If a feature can't be expressed as "what does the next single `loto`
invocation do, given the current state of `$LOTO_HOME`?", it's probably
in the wrong layer.

## end-state acceptance

The north star is reached when a fresh Claude, dropped into any worktree
of a project where 4 other Claudes are working, can:

1. Run `loto status --json` and understand who's on what in <1s.
2. Stake a reservation, acquire a file lock, and edit safely.
3. Receive a useful blocker report when something is held.
4. Send and receive messages without setup beyond `LOTO_AGENT_ID`.
5. Crash, restart, and resume without leaving stale state.
6. Commit through a hook that catches the one mistake humans make.

That's the bar. Everything in the backlog (loto-7wp.*) is a step toward
it. Anything else is scope creep.
