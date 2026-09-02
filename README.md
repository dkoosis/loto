# loto

Lock-out / tag-out coordination for multi-agent workspaces. Stops two Claude
sessions from silently clobbering each other's edits.

## what it does

You have several Claude sessions running in the same repo — worktrees,
subagents, concurrent windows. Without coordination, two of them can edit
`internal/store/store.go` at the same time and one set of changes vanishes.

loto answers: "Is it safe for me to edit this path right now, and if not,
who's on it and what are they doing?"

Acquire a lock with intent. The tag carries your session id, PID, branch, and
the one-line intent. The next agent that tries the same file sees the
holder and decides: wait, work elsewhere, or break the lock.

```sh
loto lock internal/store/store.go -t "refactor query path"
# ✓ locked count=1
# ✓ locked target=/Users/dk/Projects/foo/internal/store/store.go

# from another session:
loto status
# (prints held rows with owner / intent / expires_at)

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
events (PreToolUse → PostToolUse), and the PreToolUse gate refuses a
peer's write over a held path. loto itself never changes a file's
permissions — see `docs/DESIGN.md` for why that changed, and for the
full design contract.

## installation

`go.mod` declares the bare module path `loto` (single-host personal
tooling, not published to a proxy) — `go install` can't resolve it from
outside a checkout. Clone and build instead:

```sh
git clone https://github.com/dkoosis/loto.git
cd loto
make install   # installs to $GOPATH/bin
```

## commands

```sh
loto lock <path>... -t "<intent>"     # acquire one or more locks atomically; -t required
loto unlock <path>... -t "<intent>"   # release; --force to break another's, --all for all mine
loto check [<path>...]                # check targets for conflicts; --staged for git staged paths
loto status                           # who holds what; --mine to filter
loto doctor [--dry-run|--repair]      # detect and optionally repair stale locks
loto whoami                           # this session's owner id; records its liveness witnesses
loto violations [scan|resolve <id>]   # unauthorized writes to unleased paths; sticky until reverted or resolved
loto gate stats [--since <dur>]       # admission verdicts per rejection class
loto version                          # version
```

Output is Claude-optimized KV — one record per line, fixed glyphs (`✓` /
`✗`), deterministic order. See `.claude/rules/design.md` for the contract.

## coordination model

| Layer | Mechanism | Truth source | Status |
|------|-----------|--------------|--------|
| Tag (record-tier) | `locks` row with non-zero, unexpired `expires_at` | row + TTL (lazy GC) | shipped |
| Enforcement (chmod) | strip-write on acquire; restore on release | filesystem mode bits | retired (loto-zssw) |
| Harness gate | PreToolUse hook refuses a write over a peer's held path | `locks` + `claims` rows | shipped in the `loto@sdlc` Claude Code plugin (`hooks/hooks.json` → `scripts/pre-tool-use.sh`), globally enabled via `~/.claude/settings.json` — a fresh clone of *this* repo gets no PreToolUse hook |
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

> **Upgrading past the identity retirement (loto-jnid).** The owner id used to
> be a uuid minted into `~/.loto/agents/<session>.json`; it is now the Claude
> Code session id itself. Locks taken by the OLD binary therefore carry an
> owner the new binary no longer resolves to, and `loto unlock` on them
> reports `state=not-owner`. They are not stuck: they free at TTL lapse, on
> liveness reclaim once the old session is gone, or immediately via
> `loto unlock --force`. Nothing else migrates — `~/.loto/agents/` and
> `~/.loto/peers/` are dead data after the upgrade and can be deleted
> (`rm -rf ~/.loto/agents ~/.loto/peers`).

## on-disk layout

```
$XDG_STATE_HOME/loto/
└── projects/<slug>/        # one per project (derived from git remote)
    ├── loto.db             # SQLite: locks
    └── lock-op.flock       # short-lived DB op serializer

~/.loto/
└── session/<sid>.json      # per-session liveness witnesses (pid, start-time, socket)
```

`LOTO_BASE` overrides **both** roots — lock store at `$LOTO_BASE/`, session
records at `$LOTO_BASE/session/`.

The project slug is derived from `git remote get-url origin` (normalized).

## exit codes

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | advisory conflict (lock held by another agent) |
| 2 | usage error |
| 3 | IO / system error |

## session identity

The owner of every lock is the Claude Code session id: `CLAUDE_CODE_SESSION_ID`,
which Claude Code exports to every shell-out from one session. Nothing is
minted and nothing is stored to get there — the same id is what `ListAgents`
and `SendMessage` address, so a blocker report names a peer you can message
directly. `LOTO_AGENT_ID=<uuid>` pins an explicit owner instead; a caller with
neither (a bare shell, codex, `claude -p`) can read lock state but is refused
on every verb that would write an owner.

`loto whoami`, run by the SessionStart hook, records the session process's
pid, start-time and messaging socket at `~/.loto/session/<sid>.json`. That is
the evidence `loto lock`/`check`/`doctor` use to tell a holder whose session
is still up from one whose session died, before the TTL backstop.

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
| Lock means stop, physically | the harness gate approximates it; advisory at root, and it only reaches tools the hook matches |

The gap between the metaphor and the code is the standing design backlog, not
an accident of naming.

## design invariants

1. **flock + filesystem are truth, with one bounded exception.** Never
   trust a SQL row for the safety of a foreground operation; rows with a
   non-zero, unexpired `expires_at` are authoritative for that TTL window.
2. **Single host.** Canonical paths on this machine. No NFS, no remote.
3. **No daemon.** State lives on disk; every operation is a fresh process.
4. **Claude-optimized KV output.** Deterministic order, fixed glyphs.
5. **Identity is per-session, not per-process.** Many shells, one owner id.
6. **Reads are free.** loto coordinates writes only.
7. **Cleanup is layered.** SessionEnd hook (eager) → lazy GC on next
   acquire (passive) → `loto doctor --repair` (manual).

See `docs/DESIGN.md` for the full contract.

## what loto isn't

- **Not multi-host.** flock(2) on NFS is unreliable; do not use over network mounts.
- **Not a daemon.** Fresh process per op, state on disk.
- **Not strongly consistent.** Cooperative coordination; bypassable by `chmod +w` / `sudo`.
- **Not a git conflict resolver.** loto reduces conflicts; git handles them.

## development

```sh
make check    # fmt + vet + test + build
make test     # go test -json -count=1 -cover ./...
make race     # go test -race -json -timeout=20m -count=1 ./...
make install  # install to $GOPATH/bin
```
