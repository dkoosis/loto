# bundle-static — loto-ux3.1

Frozen at P-write. Pass-N specialists receive this verbatim plus `bundle-dynamic-pass-N.md`.

## bead spec
See `bd show loto-ux3.1 --json` (snapshotted in plan.md). Title: *loto acquire / release <path>: process-independent hold primitive*.

## profile + rules
- **profile:** craft (default loto rules)
- **design rules:** `.claude/rules/design.md` — stdout audience = Claude; deterministic, glyphs ✓ ✗ ‡ →, JSON when not tty, exit codes stable.
- **boot:** `.claude/rules/boot.md` — magloop sweep done; lint cache trap noted.

## north star (excerpt)
`docs/NORTH_STAR.md`. Key claims this bead must respect:
- Tags are descriptive, **flock is authoritative**.
- No daemon, no scheduled sweep — TTL must be checked **lazily on access**.
- Identity is host-global (`~/.loto/agents/<uuid>.json`); state is project-scoped (`$XDG_STATE_HOME/loto/projects/<slug>/`).
- Exit codes: 0 success · 1 advisory conflict · 2 usage · 3 IO/system. Bead overlays code 3 = `--wait` timeout (matches `try` semantics — verify against existing try code).
- Output: JSON when not tty; holder identity rides on errors.

## existing surfaces this touches
- `acquire.go` — already defines `LOTO.Acquire(ctx,…)` and `LOTO.AcquireGlobal`. **Name collision** with proposed CLI subcommand. Today's Go-level `Acquire` is a *blocking poll* wrapping `TryFileLock` (foreground only); the bead wants a *record-write* primitive that returns immediately. Plan must address whether the new CLI calls a new method (`AcquirePersistent`?) or whether this is a fundamentally different operation.
- `loto.go` — `TryFileLock`, `TryGlobalLock`, `Tag` struct. TTL field does not currently exist on `Tag`.
- `release.go` — `ReleaseAllMine(agentID)` + `reapTagIfMine`. No per-path release-by-target today; existing `loto release <handle>` takes a handle, not a path. Bead asks for `loto release <path>` semantics — clarify in plan whether this overloads the existing subcommand or is a new entry point.
- `cmd/loto/main.go` — root `AddCommand` block; bead adds `acquire`, possibly extends `release`.
- `cmd/loto/reserve.go` — closest analogue (record-based, no foreground).
- `internal/render/llm.go` — emit format for new commands.
- Tests: `loto_test.go`, `release_test.go` (?), `cmd/loto/integration_test.go`.

## acceptance (from bead)
1. `loto acquire /tmp/x` returns 0, records held-state, exits.
2. Subsequent `loto try file /tmp/x` from another agent returns 1 with original acquirer's identity.
3. `loto release /tmp/x` from same agent clears the record; subsequent `try` succeeds.
4. TTL expiry: `--ttl 1s`, after 1s, another agent's `try` succeeds with no manual release.
5. Cross-agent release: returns non-zero with clear error (no silent steal).
6. `loto release --all-mine` continues to work unchanged.
7. Idempotent re-acquire by same agent extends TTL, returns 0.
8. Per-path release when not held: returns 0 silently (hook robustness).

## "excellent vs acceptable" (from bead)
- TTL is a **field on the existing tag record**, not a sidecar file format.
- Acquire'd locks **indistinguishable** from `--hold` locks via `loto status` and another agent's `loto try`.
- Lazy TTL check — no daemon, no sweep.
- Single command, no `--mode persist`-style flag soup.

## downstream (this bead unblocks)
`loto-ux3.2` (hook adapter) consumes acquire/release via JSON-over-stdin. Output shape must be stable enough that hook adapter can rely on it.

## token note
This bundle ≈ 600 tokens (rough). First architectural dispatch — actual token count goes in the rehearsal banner so the 50k/stage watermark gets calibrated, not guessed.
