# loto-9k8 — final state

## Outcome

Shipped. Acceptance gate met:
- Zero new lint findings (pre-existing `intrange` in `identity/registry.go:127`
  deferred as **loto-n1h**, outside locked territory).
- All tests pass; race clean across changed packages.
- N+1 gate: `rg 'for .* := range .* \{' internal/store/locks.go internal/store/events.go internal/cli/cmd_unlock.go | rg '\.(BreakLock|releaseOne|AppendEvent)\('`
  returns zero hits for in-scope sites.

## Plan-vs-actual delta

| File | Planned? | Notes |
|------|----------|-------|
| `internal/store/events.go` | yes | `AppendEvents` + `appendEventsTx`; `AppendEvent` is now a wrapper |
| `internal/store/events_test.go` | yes | 3 new tests; `tcAGo` constant added |
| `internal/store/locks.go` | yes | `BreakLocks`/`BreakResult` added; `ReleaseLocks` single-tx; `releaseOne` removed; restore-fail audit batched; `classifyReleases`/`classifyBreaks`/`restoreAndAuditReleases`/`restoreAndAuditBreaks` extracted to keep cognitive complexity ≤ 15 |
| `internal/store/locks_test.go` | yes | 3 new tests |
| `internal/cli/cmd_unlock.go` | yes | `breakTargets` calls `BreakLocks` once |
| `internal/store/testconsts_test.go` | not in plan | Added `tcAGo = "a.go"` to satisfy goconst — trivial test-only constant, kept in scope |
| `.claude/settings.local.json` | not in plan | Added `worktree.bgIsolation = none` per user's "single repo, no worktrees" directive |

## Out-of-scope sites (deferred)

- **F4 — DoctorRepair post-commit audit loop** (`internal/store/doctor.go:120`):
  `restoreAndAudit` still loops per failure. Not in my locked territory. Now
  trivially fixable by replacing the loop with one `AppendEvents` call; helper
  primitive is in place.
  → Filed follow-up suggested but not yet created — recommend filing as
  `cleanup: migrate DoctorRepair.restoreAndAudit loop to AppendEvents` (next).

## Findings filed during this bead

- **loto-n1h** — `for i := 0; i < 20` → `range 20` in `internal/identity/registry.go:127`. Pre-existing on main.

## north_star_answer

dk: loto-w0s messaging primitives not in this build → `loto inbox`/`send` commands unavailable; skipped multi-agent ping steps from the dispatch prompt. Coordination relied on lock acquisition only.

## Receipt

- Plan: docs/superpowers/plans/loto-9k8/plan.md
- Audit: docs/superpowers/plans/loto-9k8/stage-0-audit-clean.log (only pre-existing identity/registry finding, filed as loto-n1h)
- Tests: 6 new (3 events, 3 locks) + race clean
