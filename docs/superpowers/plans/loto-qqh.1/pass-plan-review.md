# pass-plan-review — loto-qqh.1

**Reviewed:** `docs/superpowers/plans/loto-qqh.1/plan.md`
**Spec:** `docs/superpowers/specs/2026-05-10-lockout-primitive-design.md`
**Date:** 2026-05-10
**Reviewer:** feature-dev:code-reviewer subagent

---

## Critical (P0)

None.

---

## P1 — fix before approve

**P1-A · Task 4 Step 3 · Store struct in Task 4 body drops `stderr` field**
- Section: Task 4, Step 3, first code block
- Issue: The verbatim `type Store struct { db *sql.DB; dbPath string }` block overwrites the `stderr io.Writer` field established in Task 3. Implementer note #1 warns about incremental application, but a mechanical reader of the plan body will delete `stderr` at this step.
- Fix: Remove the `type Store struct` block from Task 4 Step 3 entirely. Replace with: "Keep the `Store` struct from Task 3 unchanged; only add the `opFlockPath()` method below."

**P1-B · Task 1 Step 3 · Contradictory `PRAGMA user_version` placement**
- Section: Task 1, Step 3
- Issue: Plan body says append at end; implementer note #2 says after header comment, before `CREATE TABLE` blocks. SQLite executes either correctly, but the contradiction produces a schema.sql that differs from the stated convention and looks wrong in code review.
- Fix: Drop "append at the end" from Step 3. State position once: "after the header comment, before the first `CREATE TABLE`."

**P1-C · Task 3 Step 1 · Serialization test proves count, not mutual exclusion**
- Section: Task 3, `TestOpFlock_SerializesConcurrentHolders`
- Issue: The test asserts `len(order) == 3`. A no-op implementation (flock body removed) also passes — three goroutines all append and complete regardless of serialization. The test name claims serialization but provides no evidence of it.
- Fix: Inside each goroutine's flock hold, record entry/exit timestamps. After `wg.Wait()`, assert that no two hold windows overlap. Or: use a shared `int32` counter via `atomic.AddInt32`; inside the hold check the counter is exactly 1, then decrement — any concurrent pair sees 2.

---

## P2 — follow-up bead

**P2-A · `stripWrite` missing-file behavior untested**
- Section: Task 2, `chmod_test.go`
- Issue: `restoreWrite` has an explicit `TestRestoreWrite_MissingFileIsNoop` test. `stripWrite` on a missing path returns `fs.ErrNotExist` via `os.Stat`, but this is not documented or tested. The asymmetry (restoreWrite = no-op, stripWrite = error) is spec-correct but invisible to the test suite.
- Fix: Add `TestStripWrite_MissingFileReturnsError` to `chmod_test.go`.

---

## P3 — note only

**P3-A · `sync.Once` sole reason for `sync` import in `flock.go`**
- Section: Task 3, `flock.go` imports
- Issue: `sync` imported solely for `noticed sync.Once`. No problem now, but a future refactor that changes the notice pattern would leave a dangling import. Flag in code review.

---

## Scope check

No out-of-scope leakage found. AcquireLock/ReleaseLock/BreakLock body changes are correctly deferred to loto-qqh.2/.3. `opFlockPath()` is correctly exposed without being wired into the public API. Doctor, render, and CLI are untouched.

## TDD check

All four tasks show explicit fail-first steps with expected FAIL output before implementation steps. Discipline is intact.

## Spec alignment check

- chmod policy: `mode &^ 0o222` (stripWrite) and `mode | 0o200` (restoreWrite) — matches spec exactly.
- Lossy restore acknowledged in `restoreWrite` doc comment — matches spec §"chmod policy (no stored mode)".
- Build-tag isolation `//go:build unix` on both `flock.go` and `flock_test.go` — specified and present.
- `ErrFlockTimeout` exported — correct; sibling beads need `errors.Is` access.
- No schema column additions — correct per spec §"schema change: None."

---

> Re-read `docs/NORTH_STAR.md`. Does this plan keep loto on course toward its north star? If yes, name the JTBD it serves. If no, name the drift in one sentence.

Yes. JTBD: **"Is it safe for me to edit this path right now?"** Tasks 1–4 build the enforcement substrate — schema integrity, chmod primitives, op-flock serialization, path derivation — that converts the existing advisory tagout row into a real enforcement tier. Without this foundation, the green checkmark has no teeth; this bead is precisely what NORTH_STAR tier-2 ("Acquire, blocks") requires to become enforceable.
