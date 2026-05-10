# Plan — loto-ux3.6 — `--on-timeout {block,warn,switch}`

## Goal
Replace cryptic wait-timeout output with explicit policy modes selected via `--on-timeout`. Apply to both `loto try file/global --wait` and `loto acquire --wait`. Output uses an explicit `✗ timeout` line so callers can parse the policy decision without consulting exit codes alone.

## Today's behavior (verified)
- `loto try ... --wait` against held → `pollAcquire` returns last `ErrHeld`, `exit()` emits `EmitLLMBlocked` with `✗ blocked`, exits **1**. (Bead description's claim of exit 3 here is wrong; only `acquire`'s `emitWaitTimeout` exits 3.)
- `loto acquire --wait` against held → `emitWaitTimeout` emits `EmitLLMBlocked` (`✗ blocked`), exits **3**.
- Both paths emit blocked-shaped output; neither distinguishes "deadline passed" from "tried-once-and-lost."

## New flag
```
--on-timeout block   (default — exit 3, hard timeout)
--on-timeout warn    (exit 0, structured warn on stderr, proceed-but-noted)
--on-timeout switch  (exit 1, structured suggestion: msg-and-switch)
```
Applies to `loto try file --wait`, `loto try global --wait`, `loto acquire --wait`. Without `--wait`, `--on-timeout` is silently ignored (no-op; non-blocking try has no timeout).

## Output

LLM (one line, on stderr):
```
✗ timeout | <kind> | <target> | by:<handle> | intent:<intent> | suggested-action:<abort|proceed|msg-and-switch>
```
(Optional held-since/expires-at appended if known, matching `EmitLLMBlocked` field order.)

JSON (stderr):
```json
{"timeout":true,"policy":"switch","kind":"file","target":"...","agent_id":"...","intent":"...","suggested_action":"msg-and-switch"}
```

## Exit codes
- `block` → 3 (hard timeout, current `acquire` behavior)
- `warn` → 0 (proceed)
- `switch` → 1 (policy suggestion / soft conflict)
- Unknown value → 2 (usage)

‡ Back-compat note: today `try --wait` exits 1 on timeout (because `pollAcquire` returns ErrHeld). Default `block` will change that to **3**, matching `acquire`. Bead acceptance explicitly states default should be exit 3.

## Files edited

| File | Change |
|------|--------|
| `internal/render/llm.go` | Add `EmitLLMTimeout(w, in BlockedInput, suggestedAction string)` — emits `✗ timeout` line; reuses BlockedInput |
| `internal/render/llm_test.go` | Golden tests for timeout emit (3 actions) |
| `cmd/loto/acquire.go` | Add `--on-timeout` flag; replace `emitWaitTimeout` with shared `emitTimeout(in, mode)` |
| `cmd/loto/main.go` | Add `--on-timeout` flag to `try file/global`; intercept ErrHeld from `pollAcquire` when wait+flag set, route through `emitTimeout`; `parseOnTimeout` helper validates value |
| `cmd/loto/acquire_test.go` or new `cmd/loto/on_timeout_test.go` | Integration tests: 3 modes × {try, acquire} = 6 cases + 1 usage rejection |
| `docs/NORTH_STAR.md` | Document flag + exit-code semantics |

## Out of scope (deferred)
- cc-plugins `loto-coordinate` skill update — separate repo (`~/.claude/plugins/cc-plugins`), file as follow-up bead loto-ux3.6-skill.

## TDD order
1. **Red**: `TestEmitLLMTimeout_*` for three suggested-action variants (golden).
2. **Green**: implement `EmitLLMTimeout`.
3. **Red**: integration test `TestTryFileWaitOnTimeoutSwitch` — held file, `--wait 1s --on-timeout switch`, expect exit 1 + `✗ timeout` + `suggested-action:msg-and-switch`.
4. **Green**: wire flag into `tryCmd`, route ErrHeld from pollAcquire through `emitTimeout`.
5. **Red**: same for `warn` (exit 0) and `block` (exit 3, default).
6. **Green**: complete the dispatch.
7. **Red**: same triplet for `loto acquire`.
8. **Green**: refactor `emitWaitTimeout` → `emitTimeout(mode)`.
9. **Red**: `--on-timeout bogus` → exit 2.
10. **Green**: `parseOnTimeout` validation.
11. **Polish**: `make audit`. Doc update.

## Acceptance checklist
- [ ] `loto try file <held> --wait 1s --on-timeout switch` → exit 1, `✗ timeout | file | <path> | by:<h> | ... | suggested-action:msg-and-switch`
- [ ] `--on-timeout warn` → exit 0, structured warn on stderr
- [ ] `--on-timeout block` (default) → exit 3, structured timeout on stderr
- [ ] `--on-timeout bogus` → exit 2 with usage error
- [ ] All three modes work for `loto acquire --wait` symmetrically
- [ ] Without `--wait`, flag is no-op (no error, doesn't affect non-blocking)
- [ ] `make audit` clean
