---
name: loto
description: >
  Use when about to edit a file in a project where multiple Claude sessions
  may be running (worktrees, subagents, concurrent windows), or before any
  large refactor that touches many files. Coordinates file locks to prevent
  silent clobbers. Triggers: "edit X", "refactor Y", "modify Z" in shared
  repos; "I'm starting a sweep across …"; conflict-shaped errors after an
  edit; "who has X locked?"; "what am I called in this project?"; "triage
  backlog", "pick a bead", "bd ready", "claim a bead", "drain the queue"
  in a multi-agent repo.
---

# loto — multi-agent file coordination

‡ Output is plain text, leading glyph per line (`✓` ok, `✗` conflict). Parseable: `key=value` fields, deterministic sort, paths relative to cwd. See "Reading output" below.

## Verbs at a glance

| Verb | What it does | Exit codes |
|------|--------------|------------|
| `lock <paths…> -t "<intent>"` | Acquire advisory lock on regular files. `-t` required. | 0 ok / 1 conflict / 2 usage / 3 io |
| `unlock <paths…> [-t "<why>"] [--force] [--all]` | Release your locks. `--force` breaks a peer's (give `-t`). `--all` releases all yours. | 0 / 2 / 3 |
| `check <paths…>` | Silent probe: exit 0 means no peer holds any path. `--staged` reads git staged paths. | 0 free / 1 held / 2 / 3 |
| `status [<paths…>] [--mine]` | Per-target table of holders + intents. | 0 / 2 / 3 |
| `doctor` | Detect / repair stale locks. | 0 / 3 |
| `whoami` | Print agent identity (name + id). | 0 |
| `version` | Print loto version. | 0 |

‡ Targets must be **regular files**. Globs and directory locks were cut (PR #65). Lock one anchor file per subtree instead.

## When to use

- About to edit a file where another Claude session may be active (worktrees, named subagents, multiple windows).
- Before a multi-file sweep — `check` the blast radius first, then `lock` anchor files.
- Surprising diffs ("I didn't write that") → `loto status` to find the holder.

## Operating loop

```bash
loto whoami                              # orient
loto check <paths…> || loto status <paths…>   # probe; on hold, see who
loto lock <paths…> -t "<bead-id or why>"      # claim
# ... edit ...
loto unlock <paths…> -t "<bead-id> done"      # release
```

→ `lock` persists across process exits (advisory, file-backed in the per-project DB). Hold it across multiple Bash tool calls; release when the edit ships.

## Triaging `bd ready` in a multi-agent session

`bd ready` doesn't consult peer locks. Filter before claiming.

```bash
bd ready --json | jq -r '.[] | "\(.id)\t\(.metadata.blast_paths // "")"' | {
  count=0
  while IFS=$'\t' read -r id paths; do
    count=$((count + 1))
    if [ -z "$paths" ]; then
      echo "⚠ unverified $id (no blast_paths metadata)"
      continue
    fi
    IFS=',' read -ra path_array <<< "$paths"
    if loto check "${path_array[@]}" >/dev/null 2>&1; then
      echo "✓ claimable $id"
    else
      echo "✗ blocked $id"
    fi
  done
  [ "$count" -eq 0 ] && echo "∅ bd ready empty — no candidates to triage"
}
```

If every candidate triages `✗ blocked` / `⚠ unverified`, say so to the human — silence reads as a crash.

## Multi-agent patterns

### Parallel feature lanes
Each agent locks one anchor file per subtree with an intent naming the lane.
```bash
loto lock internal/auth/auth.go -t "own auth lane — loto-abc"
loto lock internal/store/store.go -t "own store lane — loto-def"
```
Peers running `status` see the lane claim and route elsewhere.

### Backlog drain
Loop: triage → claim → edit → unlock. `check` before each claim because state moves while you work.
```bash
loto check <paths…> && loto lock <paths…> -t "<bead>"
# ... ship ...
loto unlock <paths…> -t "<bead> done"
```

### Long migration vs day-to-day
Long-running migration uses a verbose intent + a long `--ttl` so peers understand the wait; day-to-day agents reroute on sight.
```bash
loto lock db/schema.sql --ttl 4h -t "migration loto-mig.3 — ETA EOD, reroute to feature work"
```
‡ default `--ttl` is 30m; bump it when you know you'll be longer. peers see `expires_at=` in `status`/`check` output.

### Reviewer + author
Reviewer treats `status` output as an in-flight signal — author still holds → defer review.
```bash
loto status <files-in-PR>      # held by author? wait. free? review now.
```

### TDD pair (red → green handoff)
Red writer locks with intent=red, releases with `-t red-done`. Green peer reclaims with intent=green.
```bash
# agent A
loto lock impl.go test.go -t "red — failing test for loto-tdd.1"
# ... write failing test, commit ...
loto unlock impl.go test.go -t "red done — impl yours"
# agent B
loto lock impl.go test.go -t "green — make it pass loto-tdd.1"
```

### Cross-repo coordination
‡ Lock DB is **per-project**, keyed by git origin or directory basename. Worktrees of the same repo share via `GIT_COMMON_DIR`. Sibling repos do **not** share a lock domain.
→ For cross-repo work, coordinate out-of-band (chat / nug / bead intent). loto cannot see across project boundaries.

## Reading output

Leading glyph on every line. Fields are `key=value`, space-separated. Paths are repo-relative.

| Glyph | Meaning |
|-------|---------|
| ✓ | success / clear |
| ✗ | conflict / error |

**lock ok:**
```
✓ locked count=1
✓ target=internal/store/store.go
```

**lock or check blocked (exit 1):**
```
✗ blocked count=1
✗ target=internal/store/store.go owner=<uuid> intent="store refactor — loto-7wp.4" expires_at=2026-05-16T18:00:00Z
```
`check` also emits a ready-to-run fix block:
```bash
loto unlock --force -t "unblock" internal/store/store.go
```

**status:**
```
project: <slug>
repo:    <path>
state:   <state-dir>
✓ locks count=2
✓ target=... owner=<uuid> intent="..." held_since=... expires_at=... host=... pid=...
```

When blocked:
1. **Wait atomically** — `lock` is the probe (no TOCTOU):
   ```bash
   while ! loto lock <path> -t "<why>"; do sleep 5; done
   ```
2. **Work elsewhere** — pick another file.
3. **Break with reason** — only if the holder is gone / stuck:
   ```bash
   loto unlock <path> --force -t "holder ghosted — taking over for loto-xyz"
   ```

## Don'ts

- ✗ `loto unlock --force` without `-t "<reason>"` — the displaced agent needs to know why.
- ✗ Assume directory or glob locks work — they were removed. Lock files.
- ✗ Use any pre-commit / hook subcommand — none ship. Identity is seeded by an external Claude Code SessionStart hook setting `LOTO_AGENT_ID`.
- ✗ Expect locks to cross sibling repos — per-project DB.

## Cross-refs

- `~/Projects/loto/docs/NORTH_STAR.md` — full design rationale
- nug `32f0ece29b72` — Claude-Optimized Utility Output
- nug `c75320ff5718` — Symbol Glossary
