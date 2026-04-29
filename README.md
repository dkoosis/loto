# loto

Lock-out / tag-out coordination for multi-agent workspaces. Lets multiple
Claude sessions edit files in the same repository without clobbering each other.

## what it does

When several Claude sessions work in the same project (across worktrees,
subagents, or concurrent windows), they can unknowingly edit the same files.
loto answers one question fast:

> "Is it safe for me to edit this path right now, and if not, who's on it?"

If the answer arrives in <50ms, with structured output, with a useful holder
description, Claudes will actually use it.

> Using Claude Code? Install the loto skill at `~/.claude/skills/loto/SKILL.md`
> (snapshot at `docs/skills/loto.md`) and the global hooks at
> `~/.claude/settings.json` so every session gets identity + auto-release.

## non-goals

- **No multi-host coordination.** Designed for a single machine. flock(2)
  semantics on NFS / networked filesystems are unreliable — do not use loto
  over shared network mounts.
- **No daemon.** Every operation is a fresh process. State lives on disk.
- **No strong consistency.** Tags are advisory; flock(2) is the ground truth.
- **Not a git conflict resolver.** loto reduces conflicts; git handles them.

## installation

```sh
go install loto/cmd/loto@latest
# or build from source:
make install
```

## quick start

```sh
# Who am I in this session?
loto whoami
# → loto:llm:v1
# → agent | RemoteSnipe | id:2dd46381 | host:Mac

# Acquire a file lock (non-blocking):
loto try file internal/store/store.go
# → loto:llm:v1
# → ✔ acquired | file | internal/store/store.go | by:RemoteSnipe

# When blocked (stderr, exit 1):
# → ✗ blocked | file | … | by:GreenCastle | intent:… | held-since:…

# JSON output for scripts / hooks:
loto whoami --json

# Acquire with wait (blocks up to 30s):
loto try file internal/store/store.go --wait 30s

# Hold for foreground work:
loto try file internal/store/store.go --hold

# Check what's locked in this project:
loto status

# Release stale tag from a crashed session:
loto reap internal/store/store.go

# Release all locks for this session:
loto release --all-mine

# Install Claude Code session hooks:
loto install-hook

# Reserve a subtree (advisory):
loto reserve add "internal/store/**" --intent "refactoring store layer"
loto reserve list
loto reserve release "internal/store/**"

# Install git pre-commit hook (blocks commits on conflicting locks/reservations):
loto install-git-hook
```

By default, loto emits the **claude-optimized** terse format when stdout is
not a tty (≈40-60% fewer tokens than JSON). Pipe consumers and existing
hooks should pass `--json` explicitly.

## coordination model

Three tiers, weakest to strongest:

| Tier | Mechanism | Truth source | Use case |
|------|-----------|--------------|----------|
| Reservation | `reservations/<hash>.tag` | tag presence | "I plan to work in `internal/store/**` for an hour" |
| File lock | `flock(2)` exclusive + tag | flock | "I am editing this specific file right now" |
| Global lock | `flock(2)` exclusive (all) | flock | "Sweep across the whole tree; everyone stand down" |

**Tags are descriptive; flock is authoritative.** Tags can lie (writer
crashed, tag rotted). flock cannot — if you can acquire it, nobody holds it.

## operating loop

```
1. orient    → loto whoami            # who am I in this session?
2. acquire   → loto try file <path>   # exit 0 + JSON on success, exit 1 + holder JSON on conflict
3. edit      → ... do the work ...
4. release   → loto release --all-mine
```

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
| 2 | usage error (bad flags, unknown command) |
| 3 | IO / system error |

## session identity

Each Claude session gets a persistent handle stored at
`~/.loto/agents/<uuid>.json`. Set `LOTO_AGENT_ID` in the environment to
re-attach to an existing identity. The `install-hook` command writes
`SessionStart`/`Stop` hooks to `.claude/settings.json` to handle this
automatically.

```sh
loto install-hook   # write .claude/settings.json hooks
```

## design invariants

1. **flock is truth.** Never trust a tag alone for a safety decision.
2. **Single host.** All paths are absolute on this machine.
3. **No daemon.** State lives on disk; every operation is a fresh process.
4. **Claude-optimized I/O.** Default to terse `loto:llm:v1` format on non-tty stdout; `--json` for scripts; pretty for ttys.
5. **Identity is per-session, not per-process.** Many shells, one handle.
6. **Reads are free.** loto coordinates writes only.
7. **Cleanup is layered.** SessionStop hook (eager) → lazy GC on next acquire (passive) → `loto doctor --repair` (manual).

## known limitations

- `loto try file` in fire-and-return mode (no `--hold`) releases the lock
  immediately after printing. Persistent hold-while-working requires `--hold`
  or a wrapper that keeps the process alive. Full handle-based release is
  tracked in the backlog.
- Reservation and mailbox tiers are not yet implemented.
- `loto doctor` is not yet implemented.

## development

```sh
make check    # fmt + vet + test + build
make test     # go test -race ./...
make install  # install to $GOPATH/bin
```
