# loto

Lock-out / tag-out coordination for multi-agent workspaces. Stops two Claude
sessions from silently clobbering each other's edits.

## what it does

You have several Claude sessions running in the same repo — worktrees,
subagents, concurrent windows. Without coordination, two of them can edit
`internal/store/store.go` at the same time and one set of changes vanishes.

loto answers, in <50ms:

> "Is it safe for me to edit this path right now, and if not, who's on it
> and what are they doing?"

Acquire a lock with intent. The tag carries your handle, PID, branch, and a
one-line "what I'm doing." The next agent that tries the same file sees the
holder and decides: wait, message them, work elsewhere, or break the lock.

```sh
loto try file internal/store/store.go --hold --intent "refactor query path"
# → ✔ acquired | file | internal/store/store.go | by:BraveOtter

loto status internal/store/store.go
# → ✗ held | file | internal/store/store.go | by:BraveOtter | intent:refactor query path | since:14:32

loto msg internal/store/store.go "need 5min when you're done"
# → leaves a note in the holder's mailbox; they read it with `loto inbox --mine`
```

Reservations declare advisory holds on subtrees ahead of locking
(`internal/store/**`). The `dashboard` command shows live state. `doctor` and
`break` recover from crashed holders.

> Using Claude Code? Install the loto skill at `~/.claude/skills/loto/SKILL.md`
> (snapshot at `docs/skills/loto.md`) and the global hooks via
> `loto install-hook` so every session gets identity + auto-release.

## installation

```sh
go install loto/cmd/loto@latest
# or build from source:
make install
```

## commands

```sh
loto whoami                                 # session identity
loto try file <path> [--hold] [--wait 30s]  # acquire (non-blocking by default)
loto status [path...]                       # who holds what
loto release --all-mine                     # drop my locks
loto msg <path> "..."                       # message the holder
loto inbox --mine                           # read messages addressed to me
loto reserve add "internal/store/**" --intent "refactoring store layer"
loto reserve list
loto check-paths <path...>                  # used by the git pre-commit hook
loto doctor [--repair]                      # diagnose / clean stale state
loto break <path> [--force]                 # remove stale tag, or take a live lock
loto dashboard                              # live TUI
loto install-hook                           # write Claude Code SessionStart/Stop hooks
loto install-git-hook                       # write .git/hooks/pre-commit
```

Default output is the terse `loto:llm:v1` format on non-tty stdout (~40-60%
fewer tokens than JSON). Pipe consumers should pass `--json` explicitly.

## coordination model

Three tiers, weakest to strongest:

| Tier | Mechanism | Truth source | Use case |
|------|-----------|--------------|----------|
| Reservation | `reservations/<hash>.tag` | tag presence | "I plan to work in `internal/store/**` for an hour" |
| File lock | `flock(2)` exclusive + tag | flock | "I am editing this specific file right now" |
| Global lock | `flock(2)` exclusive (all) | flock | "Sweep across the whole tree; everyone stand down" |

**Tags are descriptive; flock is authoritative.** Tags can lie (writer
crashed, tag rotted). flock cannot — if you can acquire it, nobody holds it.

## on-disk layout

```
$XDG_STATE_HOME/loto/
└── projects/<slug>/              # one per project (derived from git remote)
    ├── global.lock               # flock'd shared/exclusive
    ├── global.tag                # JSON: who holds global, why
    ├── files/<sha256>.{lock,tag} # one pair per target file
    └── reservations/<sha256>.tag # advisory glob reservations

~/.loto/agents/<uuid>.json        # host-global session identity
```

The project slug is derived from `git remote get-url origin` (normalized) and
pinned to `.git/.loto-slug` on first use. Override with `LOTO_BASE`.

## exit codes

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | advisory conflict (lock held by another agent) |
| 2 | usage error |
| 3 | IO / system error · `--wait` timeout (default `--on-timeout block`) |

`loto try --wait` and `loto acquire --wait` accept `--on-timeout {block,warn,switch}` to
choose what happens when the wait elapses. `block` (default) exits 3, `warn` exits 0
with a structured warning on stderr, `switch` exits 1 with a `suggested-action:msg-and-switch`
hint for callers running a tiebreaker policy.

## session identity

Each Claude session gets a persistent handle stored at
`~/.loto/agents/<uuid>.json`. Set `LOTO_AGENT_ID` to re-attach. The
`install-hook` command wires `SessionStart`/`Stop` hooks in
`.claude/settings.json` so identity is set and locks are released
automatically.

## design invariants

1. **flock is truth.** Never trust a tag alone for a safety decision.
2. **Single host.** All paths are absolute on this machine.
3. **No daemon.** State lives on disk; every operation is a fresh process.
4. **Claude-optimized I/O.** Terse `loto:llm:v1` on non-tty stdout; `--json` for scripts; pretty for ttys.
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes only.
7. **Cleanup is layered.** SessionStop hook (eager) → lazy GC on next acquire (passive) → `loto doctor --repair` (manual).

## what loto isn't

- **Not multi-host.** flock(2) on NFS is unreliable; do not use over network mounts.
- **Not a daemon.** Fresh process per op, state on disk.
- **Not strongly consistent.** Tags are advisory; flock is the ground truth.
- **Not a git conflict resolver.** loto reduces conflicts; git handles them.

## development

```sh
make check    # fmt + vet + test + build
make test     # go test -race ./...
make install  # install to $GOPATH/bin
```
