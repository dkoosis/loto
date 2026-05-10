# Final state — loto-ux3.6

## Summary
Added `--on-timeout {block,warn,switch}` to `loto try file/global --wait` and `loto acquire --wait`. Replaces the previous "blocked-style report on timeout" with an explicit `✗ timeout` line carrying a `suggested-action:` field. Default `block` exits 3 (matching prior `acquire` behavior); `warn` exits 0 with proceed-but-noted; `switch` exits 1 with `msg-and-switch` so a caller's tiebreaker policy can act declaratively.

## Plan-vs-actual delta
- ✓ All planned files edited as listed in plan.md.
- ✗ Did **not** create the proposed JSON-rendering helper as a separate function — folded into `emitTimeout` directly since it was straight-line code; consolidating gained no clarity.
- ✓ cc-plugins skill update deferred (out-of-repo) — file as `loto-ux3.6-skill` follow-up bead.

## Files changed
- `internal/render/llm.go` — added `EmitLLMTimeout`
- `internal/render/llm_test.go` — golden tests for timeout (3 actions); added test consts to satisfy goconst
- `cmd/loto/on_timeout.go` — new file: `parseOnTimeout`, `emitTimeout`, `maybeEmitTimeout`, policy table
- `cmd/loto/on_timeout_test.go` — new file: 4 integration tests covering all modes + invalid + no-wait noop
- `cmd/loto/main.go` — `--on-timeout` flag on `try file/global`, calls `maybeEmitTimeout` before `exit(err)`
- `cmd/loto/acquire.go` — `--on-timeout` flag on `acquire`; `acquireWithWait` takes mode; removed local `emitWaitTimeout` (now uses shared `emitTimeout`)
- `README.md` — exit-code table updated, flag documented

## Verification
- `go test ./...` → all pass (loto, cmd/loto, internal/render)
- `make audit` → 7 pre-existing lint findings remain (err113, exhaustive, funlen, gocognit in package-loto root files; goconst for "acquired"/"kind" pre-existing in cmd/loto). **Zero new findings introduced by this change.** Stale `loto-ux3.4` lint cache cleared via `golangci-lint cache clean`.

## Findings summary
- Total: 0 from review (self-review only — no specialist passes for this size)
- Applied: 0 · Rejected: 0 · Deferred: 0
- Escalated: 0

## Deferred follow-ups
- `loto-ux3.6-skill` (to be filed): update cc-plugins `loto-coordinate` skill to call `loto try --on-timeout switch` instead of inline-prescribing the msg-and-switch pattern. Out-of-repo.

## north_star_answer
NS Q: does this keep loto's stdout-as-Claude-API contract while reducing parsing burden?
A: yes — the new `✗ timeout | … | suggested-action:<x>` line is one parse, deterministic order, no ANSI, callers can branch on the action token rather than parsing exit codes plus stderr text. Same shape as `✗ blocked` (only the marker differs), so existing field-extraction code keeps working.
