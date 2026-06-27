# loto

Lock-out / tag-out coordination for multi-agent workspaces. Stops two Claude
sessions from silently clobbering each other's edits.

## what it does

You have several Claude sessions running in the same repo — worktrees,
subagents, concurrent windows. Without coordination, two of them can edit
`internal/store/store.go` at the same time and one set of changes vanishes.

loto answers: "Is it safe for me to edit this path right now, and if not,
who's on it and what are they doing?"

Acquire a lock with intent. The tag carries your handle, PID, branch, and
the one-line intent. The next agent that tries the same file sees the
holder and decides: wait, work elsewhere, or break the lock.

```sh
loto lock internal/store/store.go -t "refactor query path"
# ✓ locked count=1
# ✓ locked target=/Users/dk/Projects/foo/internal/store/store.go

# from another session:
loto status
# (prints held rows with handle / intent / expires_at)

loto unlock internal/store/store.go -t "done"
```

When something is held by another agent, `lock` exits 1 and prints blocker
rows so you can see who and why:

```sh
loto lock internal/store/store.go -t "fix bug"
# ✗ blocked blockers=1
# ✗ blocker=BraveOtter target=/Users/dk/Projects/foo/internal/store/store.go intent="refactor query path" ...
```

Enforcement is layered: the row + TTL is authoritative across CC hook
events (PreToolUse → PostToolUse), and chmod-strip on acquire defeats
naive writers and editors that honor perms. See `docs/NORTH_STAR.md` for
the full design contract.

## installation

```sh
go install loto/cmd/loto@latest
# or build from source:
make install
```

## commands

```sh
loto lock <path>... -t "<intent>"     # acquire one or more locks atomically; -t required
loto unlock <path>... -t "<intent>"   # release; --force to break another's, --all for all mine
loto check [<path>...]                # check targets for conflicts; --staged for git staged paths
loto status                           # who holds what; --mine to filter
loto doctor [--dry-run|--repair]      # detect and optionally repair stale locks
loto whoami                           # session identity
loto version                          # version
```

Output is Claude-optimized KV — one record per line, fixed glyphs (`✓` /
`✗`), deterministic order. See `.claude/rules/design.md` for the contract.

## coordination model

| Layer | Mechanism | Truth source | Status |
|------|-----------|--------------|--------|
| Tag (record-tier) | `locks` row with non-zero, unexpired `expires_at` | row + TTL (lazy GC) | shipped |
| Enforcement (chmod) | strip-write on acquire; restore on release | filesystem mode bits | shipped |
| Op-flock (internal) | flock on `lock-op.flock`, held only during an op | flock | shipped |
| File flock (foreground) | flock(2) exclusive held by the editing process | flock | deferred |
| Global lock | flock(2) on a project-wide handle | flock | deferred |

**Truth, not tags — with one bounded exception.** flock and filesystem
mode bits cannot lie; SQL rows can (writer crashed, row rotted past TTL).
Exception: rows with a non-zero, unexpired `expires_at` are authoritative
for that TTL window — the record-tier carve-out that bridges CC hook
events without a daemon.

### Self-healing locks (liveness-primary, TTL backstop)

A lock frees the instant its owner is provably gone — no manual `loto doctor`:

- **Liveness-primary.** Each lock stamps the owning **session** pid (`LOTO_PID`,
  exported by the SessionStart hook — NOT the one-shot CLI pid) plus the
  process start-time (`proc_start`, defeats PID reuse). On any `loto lock` or
  `loto check`, a holder whose session is provably dead is reclaimed in-line.
  A clean session exit releases eagerly via the SessionEnd hook.
- **TTL backstop.** `--ttl` (default 30m) bounds the residual cases liveness
  can't cover: no durable `LOTO_PID` (bare shell / cron), cross-host rows, or a
  store that crossed a host reboot. Generous by design — the backstop, not the
  path.
- **Mid-edit expiry.** A live session (durable PID, probe alive) is NEVER
  TTL-reaped, so a long edit past the TTL is safe. Only an UNKNOWN holder
  (PID-0 sentinel) can expire mid-edit; extend it by re-running `loto lock` on
  the same target (the owner-match upsert refreshes the TTL), or fix the
  SessionStart hook to export `LOTO_PID` (promoting it to alive). loto warns at
  acquire when liveness has degraded to TTL-only.
- **`loto status`** shows `liveness=alive|dead|unknown` and `ttl_remaining=` per
  lock so the cause of every verdict is visible. status is read-only — it never
  reaps.

## on-disk layout

```
$XDG_STATE_HOME/loto/
└── projects/<slug>/        # one per project (derived from git remote)
    ├── loto.db             # SQLite: locks
    └── lock-op.flock       # short-lived DB op serializer

~/.loto/agents/<uuid>.json  # host-global session identity
```

The project slug is derived from `git remote get-url origin` (normalized).

## exit codes

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | advisory conflict (lock held by another agent) |
| 2 | usage error |
| 3 | IO / system error |

## session identity

Each Claude session gets a persistent handle stored at
`~/.loto/agents/<uuid>.json`. Set `LOTO_AGENT_ID` to re-attach.

## what "lock-out / tag-out" means

loto is named for the OSHA-grade safety practice, and the name carries a
contract. Physical LOTO has four invariants:

1. **The lock belongs to a worker** — the individual whose hand is in the
   machine. Not the crew, not the shift. A person.
2. **The hasp model.** Every worker who is exposed applies their *own* lock to
   the same isolation point; the machine can't re-energize until the last one
   comes off.
3. **Only the worker who applied a lock may remove it** — the key stays in
   their pocket. No one clears your lock for you.
4. **A lock means stop, enforced physically.** You cannot energize the breaker
   with a padlock through it. The lock isn't a note asking for cooperation.

These are the bar the software is measured against — and where loto diverges
from them is exactly where it can bite:

| Physical invariant | loto today |
|---|---|
| Lock belongs to a *worker* | identity is per-**session** (invariant 5 below); subagents of one session collapse to one "worker" |
| Hasp — each exposed worker locks | one owner per session, no hasp; same owner re-locks its own path without conflict |
| Only the applier removes it | honored per-owner — but `unlock --all` under a shared session sweeps siblings' locks |
| Lock means stop, physically | chmod-strip approximates it; advisory at root, and CC hooks fire on Bash only (agent_shell edits bypass the gate) |

The gap between the metaphor and the code is the standing design backlog, not
an accident of naming.

## design invariants

1. **flock + filesystem are truth, with one bounded exception.** Never
   trust a SQL row for the safety of a foreground operation; rows with a
   non-zero, unexpired `expires_at` are authoritative for that TTL window.
2. **Single host.** Canonical paths on this machine. No NFS, no remote.
3. **No daemon.** State lives on disk; every operation is a fresh process.
4. **Claude-optimized KV output.** Deterministic order, fixed glyphs.
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes only.
7. **Cleanup is layered.** SessionEnd hook (eager) → lazy GC on next
   acquire (passive) → `loto doctor --repair` (manual).

See `docs/NORTH_STAR.md` for the full contract.

## what loto isn't

- **Not multi-host.** flock(2) on NFS is unreliable; do not use over network mounts.
- **Not a daemon.** Fresh process per op, state on disk.
- **Not strongly consistent.** Cooperative coordination; bypassable by `chmod +w` / `sudo`.
- **Not a git conflict resolver.** loto reduces conflicts; git handles them.

## development

```sh
make check    # fmt + vet + test + build
make test     # go test -race ./...
make install  # install to $GOPATH/bin
```
