# final-state — loto-ux3.5

Bead: loto-ux3.5 — `loto hello` (combined reserve + templated announce)
Shape: standard · Profile: craft

## Summary
New `loto hello <glob> --intent <text> [--to handles] [--ttl] [--tiebreaker|--no-tiebreaker]`
subcommand. Atomic reserve + per-sibling structured msg with stable parseable
body: `loto:llm:v1 hello | handle:X | glob:Y | intent:Z | tiebreaker:W`.
Replaces the two-step prose pattern in the loto-coordinate skill.

## Plan-vs-actual
- ✓ `cmd/loto/hello.go` — created
- ✓ `cmd/loto/hello_test.go` — created (6 tests)
- ✓ `cmd/loto/main.go` — registered `helloCmd()`
- ✓ `internal/render/llm.go` — added `HelloRecipient`, `HelloResult`, `EmitLLMHello`

No files changed outside the plan.

## Verification
- `make audit` green: vet + lint (0 issues) + test + race + govulncheck
- All 6 hello tests pass
- Manual LLM smoke: `loto hello --to GreenCastle,BlueOak` → deterministic sorted output, header sentinel, ✓ glyph, no ANSI/box-drawing

## Findings / triage
None — clean implementation, no specialist passes needed for a focused subcommand bead.

## Deferred
- Per-sibling SendMsgWith failure-injection harness — out of scope; current behavior is best-effort by inspection.
- cc-plugins loto-coordinate skill update to call `loto hello` — already filed as `loto-0fb`.

## Receipt
- Branch: `loto-ux3.5`
- Audit log: `docs/superpowers/plans/loto-ux3.5/stage-0-audit-clean.log`
- Plan: `docs/superpowers/plans/loto-ux3.5/plan.md`
