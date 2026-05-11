# Triage — loto-qqh.1 (plan-review stage)

| # | severity | finding | decision | reason |
|---|----------|---------|----------|--------|
| P1-A | high | Task 4 struct block deletes `stderr` | **applied** in plan.md | in-scope, removes ambiguity, no test risk; the verbatim block was a real footgun even with my implementer note |
| P1-B | med | Schema PRAGMA placement contradiction | **applied** in plan.md | doc-only inconsistency; trivial fix; would surface in code review otherwise |
| P1-C | high | Serialization test only counts holders | **applied** in plan.md | test was a false positive (passes with no-op flock); replaced with atomic concurrent-count assertion |
| P2-A | low | `stripWrite` missing-file behavior untested | **applied** in plan.md (not deferred) | one extra test case; pins the spec-correct asymmetry with restoreWrite |
| P3-A | note | `sync` import sole use is `sync.Once` | **rejected** | premature cleanup; not P0/P1; defer to code-review pass if it ever becomes relevant |

**Outcome:** plan v2 ready for P-approve. No findings escalated to dk; no follow-up beads filed at plan stage.

---

# Triage — loto-qqh.1 (code-review stage, post-impl)

Pass: `pass-1-plan-adherence-persistence.md` (go-bug-audit:pass3-persistence agent, two-part review).

Result: **0 P0, 0 P1, 6 lower findings.** Converged on first pass.

| # | Pri | File:Line | Finding | Decision | Reason |
|---|-----|-----------|---------|----------|--------|
| F1 | P2 | store.go:50 | TOCTOU between os.Stat and sql.Open on first Open (no op-flock around Open yet) | **defer → loto-8c5** | Out of plan scope (Tasks 1–4 don't wire op-flock around Open). Address when Task 5+ wires acquire/release. |
| F2 | P2 | store.go:38 | MoveCorruptAside silently ignores -wal/-shm rename errors; orphan WAL could resurrect rolled-back state | **defer → loto-2cw** | Pre-existing in doctor.go; not introduced by this diff. Worth a separate bead. |
| F3 | P3 | store.go:34 | Open's stderr bypasses s.stderr injector | **reject — note only** | Inherently untestable (no Store yet exists at that point); inconsequential. |
| F4 | P3 | flock.go:69 | Deadline check ordering can overshoot timeout by ≤50ms | **reject — note only** | Within poll-interval budget; test absorbs it. |
| F5 | P3 | chmod.go:30 | restoreWrite ENOENT-tolerant on Stat but not on chmod (microsecond unlink window) | **reject — note only** | Trivial if observed; not observed. |
| F6 | P3 | flock.go:50 | OpenFile won't create parent dir | **reject — note only** | Safe by current ordering (Open provisions dir before opFlockPath callers). |

Follow-up beads filed: **loto-8c5** (F1), **loto-2cw** (F2).

## Specialist-pass count: 1

Skipped second pass per craft-profile triage rule: zero P0/P1, well-scoped foundations bead, context budget pressure. Single pass + recenter sufficient.

## North-star recenter (verbatim)

> Step back. Re-read `docs/NORTH_STAR.md` tiers 3–4 (file flock + chmod). Is this diff on-direction for the lockout primitive as the north-star defines it?

**Answer:** On-direction. The diff lands tier-3/4 plumbing — project op-flock (the serializer for acquire/release transactions) and chmod helpers (the filesystem enforcement layer that makes "file lock" mean something against any tool, not just loto-aware ones). Task 1 (schema bump) clears the migration path so v2 DBs can carry lock+chmod state without v1 collision. Task 4 (opFlockPath) keeps the flock co-located with `loto.db` per spec layout. No tier-1 (reservation) or tier-2 (record-tier) work — correctly scoped to foundations. Task 5 (multi-target AcquireLock with rollback) is where these foundations get exercised; this bead is on-direction precisely because it doesn't try to ship enforcement without the rollback discipline that comes next.
