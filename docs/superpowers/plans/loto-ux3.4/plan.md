# Plan — loto-ux3.4

`loto whoami --set-handle <name>`: replace the cc-plugins `loto-coordinate` skill's reach-in to `~/.loto/agents/<HANDLE>.json` with a validated, schema-owned subcommand.

## Direction

Add a `--set-handle <name>` flag to the existing `whoami` subcommand that
1. validates the requested handle, and
2. updates the `Handle` field of the current agent's record (creating it if absent), then
3. emits the updated record exactly like a no-flag `whoami` call.

No new types, no new package, no new file. Reuse `Agent`, `agentHome`, `currentAgent`, `displayAgent`.

## Validation rules

| Input              | Result                                       |
|--------------------|----------------------------------------------|
| empty string       | reject                                       |
| contains `/` `\`   | reject (path-traversal hardening)            |
| control char (<32) | reject                                       |
| length > 32        | reject                                       |
| else               | accept verbatim (no auto-Title, no trim)     |

Error path → `*loto.ErrSystem` (Op="whoami: set-handle") so existing `exit()` produces `loto:llm:v1` envelope and exit-3.

## Files

| File                                 | Change                                                                        |
|--------------------------------------|-------------------------------------------------------------------------------|
| cmd/loto/identity.go                 | add `validateHandle(name)` + `setHandle(id, name) (*Agent, error)`            |
| cmd/loto/main.go                     | flag wiring on `whoamiCmd`; branch in RunE                                    |
| cmd/loto/identity_test.go            | unit tests for validation matrix + setHandle round-trip                       |
| cmd/loto/integration2_test.go        | integration: `whoami --set-handle Scout` then `whoami` returns Scout; `try` displays Scout |

## Tests (TDD order)

1. `TestValidateHandle` — 6 reject cases + 2 accept cases.
2. `TestSetHandle_RoundTrip` — set → read back via `currentAgent`; Handle field updated.
3. `TestSetHandle_CreatesIfMissing` — no agent record yet, --set-handle creates it with the given handle.
4. `TestIntegrationSetHandle` — exec binary; `--set-handle Scout`, then `whoami --json`, assert Handle=="Scout"; then verify `displayAgent` resolves to Scout (via try output).
5. `TestSetHandle_RejectsInvalid` — exec binary with bad inputs, assert exit-3 + error envelope.

## Acceptance (from bead)

- ✓ `loto whoami --set-handle Scout` writes agent record using internal schema; subsequent `loto whoami` returns Scout.
- ✓ `loto try`/`loto reserve`/`loto msg` from this agent display "Scout" (via existing `displayAgent` resolution).
- ✓ Validation: empty / slashes / control / length>32 rejected with clear error.
- ✓ Existing `loto whoami` (no flags) unchanged.
- ✓ cc-plugins `loto-coordinate` skill update — **deferred to a follow-up bead** (different repo; not in this worktree). final-state.md will surface this.

## Out of scope

- cc-plugins skill edit (separate repo).
- `--set-handle` re-validating existing on-disk handles.
- Listing or deleting handles (separate concern).
- Allowing handle to also be passed to `currentAgent` autoload path.
