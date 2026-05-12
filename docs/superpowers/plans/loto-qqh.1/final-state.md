# loto-qqh.1 — final state

**Outcome:** shipped. Tasks 1–4 of the gh#57 lockout primitive plan.

## Summary

Foundations for the lockout primitive: schema user_version bump (2 → 3) with
wipe-on-mismatch, chmod helpers (stripWrite/restoreWrite with injectable
chmodFn), project op-flock helper (unix, bounded wait), and Store.opFlockPath
derived from the DB path.

Three commits on branch `loto-qqh.1`:
- `415b834` schema user_version=3 + opFlockPath foundations (Tasks 1, 4)
- `572bc24` chmod helpers stripWrite/restoreWrite (Task 2)
- `b39d196` project op-flock helper unix (Task 3)

## Pipeline

| Stage | Status | Artifact |
|-------|--------|----------|
| Pre-flight audit | green | stage--1-preflight-audit.log |
| Stage 0 audit (post-impl) | green | stage-0-audit.log |
| Plan review | done | pass-plan-review.md (prior stage) |
| Pass 1 (plan-adherence + persistence) | 0 P0/P1, 6 lower | pass-1-plan-adherence-persistence.md |
| Triage | converged, 2 deferred | triage.md |
| North-star recenter | on-direction | triage.md (answer recorded) |

Specialist-pass count: 1. Second pass skipped — zero P0/P1, well-scoped
foundations bead, context budget at 87k cache tokens. Documented as profile
decision in triage.md.

## Plan-vs-actual

Files in plan, present in diff:
- internal/store/schema.sql ✓
- internal/store/store.go ✓
- internal/store/store_test.go ✓
- internal/store/chmod.go ✓ (new)
- internal/store/chmod_test.go ✓ (new)
- internal/store/flock.go ✓ (new)
- internal/store/flock_test.go ✓ (new)

Files changed outside plan: none.

Deviations from plan (intentional, not drift):
- Tasks 1 and 4 folded into commit `415b834` (plan suggested separate
  commits). opFlockPath is a one-line method on Store; co-locating with the
  user_version refactor avoided a stub-commit.
- Tests use Go 1.22+ `wg.Go` / `for i := range 3` instead of the plan's
  snippet form. Cleaner — not drift.

## Test coverage

`internal/store` coverage: 72.5% (main) → 75.8% (this branch). +3.3%.

## Follow-up beads filed

- **loto-8c5** (P2): store.Open + op-flock TOCTOU on first-ever Open.
  Address when Task 5+ wires op-flock around Open.
- **loto-2cw** (P2): MoveCorruptAside should rename `-wal`/`-shm` alongside
  the main DB. Pre-existing in doctor.go; not blocking lockout primitive.

## Escalated to dk

None.

## north_star_answer

On-direction (verbatim answer in triage.md). Foundations for tiers 3–4; no
enforcement shipped here — that lands in Task 5 with chmod rollback discipline.
