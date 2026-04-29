---
name: loto
description: >
  Use when about to edit a file in a project where multiple Claude sessions
  may be running (worktrees, subagents, concurrent windows), or before any
  large refactor that touches many files. Coordinates file/global locks to
  prevent silent clobbers. Triggers: "edit X", "refactor Y", "modify Z" in
  shared repos; "I'm starting a sweep across …"; conflict-shaped errors
  after an edit; "who has X locked?"; "what am I called in this project?".
---

# loto — multi-agent file coordination

‡ **Default output is LLM-format** (terse, `loto:llm:v1` header) when stdout is not a tty. Pass `--json` only when piping to `jq` or scripts that parsed the legacy shape.

## When to use

- Any time you're about to edit a file, *and* you suspect another Claude session may be active in the same repo (worktrees, named subagents, multiple windows).
- Before a multi-file refactor: stake a glob reservation.
- When you see surprising diffs ("I didn't write that") — run `loto status` to find out who did.

## Operating loop

‡ **Critical for Bash-tool callers:** `loto try file <path>` *without* `--hold` acquires AND releases in the same process. The lock is gone the moment the Bash command returns. You cannot hold an OS lock across separate Bash tool calls. Choose the pattern that matches your workflow:

### Pattern A — multi-file refactor (recommended for Claude)

Use **reservations**. They're advisory tags that persist across process exits and surface as warnings to other agents.

```
1. orient    → loto whoami
2. reserve   → loto reserve add "<glob>" --intent "<why>"
3. probe     → loto status <path>            # check no conflicting holder
4. edit      → ... do the work ...
5. read msgs → loto inbox <path>
6. release   → loto reserve release "<glob>"
```

### Pattern B — single-file probe before edit

Use `loto try file` as a **probe**: exit 0 means no one currently holds it; you proceed and edit immediately, accepting that the lock didn't persist. Adequate for fast edits where the race window is tiny.

```
1. loto try file <path>     # exit 0 = clear to edit, exit 1 = someone holds it
2. ... edit immediately ...
```

### Pattern C — genuine hold across a long edit

Run `loto try file <path> --hold` via `run_in_background`; it stays foreground until SIGTERM. Send the signal when done. Rare; only when Pattern A or B isn't enough.

## Reading LLM output

Format: first line `loto:llm:v1`, body lines use `|`-separated fields with leading severity glyph.

| Glyph | Meaning |
|-------|---------|
| `✔`   | success |
| `✗`   | conflict / error |
| `⚠`   | warning (e.g. reservation overlap) |
| `→`   | message / row continuation |

### Examples

**whoami:**
```
loto:llm:v1
agent | RemoteSnipe | id:2dd46381 | host:Mac
```

**try file (success):**
```
loto:llm:v1
✔ acquired | file | internal/store/store.go | by:GreenCastle
```

**try file (blocked, on stderr, exit 1):**
```
loto:llm:v1
✗ blocked | file | internal/store/store.go | by:BlueOak | intent:store refactor — see beads loto-7wp.4 | held-since:2026-04-28T14:32:11Z | ttl:2026-04-28T14:42:11Z | branch:store-refactor | host:dk-mac | pid:84231
```

When blocked, you have three actions:
1. **Wait** — `loto try file <path> --wait 30s`.
2. **Work elsewhere** — pick another file or task.
3. **Message the holder** — `loto msg <path> --to <agent> "need 5min on this"`.

**status (per-target table):**
```
loto:llm:v1
status | target | holder | intent
✔ free | a.go | - | -
✗ held | b.go | GreenCastle | store refactor
```

## Exit codes

| Code | Meaning |
|------|---------|
| 0    | success |
| 1    | advisory conflict (someone holds it) |
| 2    | usage error |
| 3    | system / IO error |

## Don'ts

- ✗ Use `--no-verify` to bypass the loto pre-commit hook. If it fires, someone else is holding what you're committing — talk to them first.
- ✗ `loto break --force` without a `--reason`. The displaced agent gets a mailbox message; give them the why.
- ✗ Hold a file lock across long-running tool calls (builds, tests). Acquire just before the edit, release just after.
- ✗ Assume `loto try file` (no `--hold`) holds the lock past the bash command. It doesn't — see Pattern A/B/C above.
- ✗ Treat `loto reserve` output as LLM-format yet — it currently emits raw JSON (loto-guy, loto-84u). Parse accordingly until those land.

## Cross-refs

- `~/Projects/loto/docs/NORTH_STAR.md` — full design rationale
- nug `32f0ece29b72` — Claude-Optimized Utility Output standard (the format)
- nug `c75320ff5718` — Symbol Glossary (the glyphs)
