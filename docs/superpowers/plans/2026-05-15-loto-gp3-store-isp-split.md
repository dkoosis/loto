# loto-gp3: ISP at consumer edge + intra-pkg locks.go split — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the arch/solid/change-smells/truthful-names theme-1 findings against `internal/store` without splitting the package or changing public method signatures.

**Architecture:** Two changes, staged in one PR.
(1) Declare caller-facing role interfaces (`LockOps`, `Health`) in `internal/store/api.go`; `*Store` satisfies both via static-assert; update each `cmd_*.go` and `runtime` to depend on the narrow interface it actually uses.
(2) Split `internal/store/locks.go` (706 LOC) intra-package by concern: `locks_acquire.go`, `locks_release.go`, `locks_break.go`, `locks_query.go`, leaving shared helpers + error types in `locks.go` (~150 LOC).

**Tech Stack:** Go 1.22+, modernc.org/sqlite, beads (`bd`), loto (`loto lock`).

**TDD note:** This bead is a refactor, not new behavior. Existing tests in `internal/store/{locks,events,doctor}_test.go` + `internal/cli/*_test.go` are the regression net. Each task's verification is `make check` (vet + test + lint). No new test code is added.

**Out of scope:** No method signatures change on `*Store`. No `EventLog` interface (no external consumer yet — YAGNI; revisit if a future inspect/inbox feature consumes events). If the god-object finding persists after this PR, a phase-2 bead will split `*Store` into role structs sharing an unexported `core`.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `internal/store/api.go` | create | Declare `LockOps`, `Health`; static-assert `*Store` satisfies both. |
| `internal/store/locks.go` | shrink | Keep error types + shared helpers (`inClause`, `inClauseStrings`, `modeRestoreFailedEvent`, `loadLocksTx`, `scanLock`, `reclaimStaleTx`). |
| `internal/store/locks_acquire.go` | create | `AcquireLocks` + acquire-only helpers. |
| `internal/store/locks_release.go` | create | `ReleaseLocks` + release-only helpers. |
| `internal/store/locks_break.go` | create | `BreakLock` + `BreakLocks` + break-only helpers. |
| `internal/store/locks_query.go` | create | `ListLocks` + `LockAt`. |
| `internal/cli/runtime.go` | modify | Add typed accessors `Locks() LockOps` and `Healthz() Health`. Keep `Store *store.Store` for now (Open/Close lifecycle). |
| `internal/cli/cmd_lock.go` | modify | Switch `rt.Store.AcquireLocks` → `rt.Locks().AcquireLocks`. |
| `internal/cli/cmd_unlock.go` | modify | Switch `rt.Store.{ReleaseLocks,BreakLocks,ListLocks}` → `rt.Locks().*`. |
| `internal/cli/cmd_doctor.go` | modify | Switch lock methods → `rt.Locks().*`; switch doctor methods → `rt.Healthz().*`. |
| `internal/cli/cmd_status.go` | modify | Switch `rt.Store.ListLocks` → `rt.Locks().ListLocks`. |
| `internal/cli/cmd_check.go` | modify | Same as cmd_status. |

`Healthz()` not `Health()` to avoid a name clash with the `Health` interface type in the same package… actually the cli package only imports the interface as `store.Health`, so `(r *runtime) Health() store.Health` is fine. Use `Health()`.

---

## Task 1: Declare role interfaces

**Files:**
- Create: `internal/store/api.go`

- [ ] **Step 1: Write `internal/store/api.go`**

```go
// Package-level role interfaces. *Store satisfies all of them; callers
// should depend on the narrowest interface they need (ISP).
//
// EventLog is intentionally omitted — its only consumer today is *Store
// itself (lock ops emit audit events transactionally). Add it here when
// an external consumer (inspect/inbox UI) appears.

package store

import (
	"context"

	"loto/internal/domain"
)

// LockOps is the lock-table contract consumed by cmd_lock, cmd_unlock,
// cmd_check, cmd_status, and the doctor's lock-survey path.
type LockOps interface {
	AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error)
	ReleaseLocks(ctx context.Context, targets []domain.Target, byAgent string) ([]ReleaseResult, error)
	BreakLock(ctx context.Context, t domain.Target, byAgent string, force bool, reason string, live domain.PidLiveProbe) error
	BreakLocks(ctx context.Context, targets []domain.Target, byAgent string, force bool, reason string, live domain.PidLiveProbe) ([]BreakResult, error)
	ListLocks(ctx context.Context) ([]domain.LockRecord, error)
	LockAt(ctx context.Context, t domain.Target) (*domain.LockRecord, error)
}

// Health is the audit/repair contract consumed by cmd_doctor.
type Health interface {
	DoctorAuditWith(ctx context.Context, thisHost string, live domain.PidLiveProbe, sc SidecarCheck) (*DoctorReport, error)
	DoctorRepair(ctx context.Context, thisHost, byAgent string, live domain.PidLiveProbe) error
	ScanOrphanModes(ctx context.Context, paths []string) ([]string, error)
	RestoreOrphanMode(paths []string) (restored []string, failures []OrphanRestoreFailure)
}

// Compile-time assertions: *Store satisfies both roles.
var (
	_ LockOps = (*Store)(nil)
	_ Health  = (*Store)(nil)
)
```

- [ ] **Step 2: Verify it compiles and assertions hold**

Run: `go build ./internal/store/...`
Expected: exit 0. If a method signature drifted, the static assert fails with the exact missing/mismatched method — fix it before moving on.

- [ ] **Step 3: Run existing store tests**

Run: `go test ./internal/store/...`
Expected: PASS (no behavior changed).

- [ ] **Step 4: Commit**

```bash
git add internal/store/api.go
git commit -m "store: declare LockOps + Health role interfaces (loto-gp3)"
```

---

## Task 2: Add typed accessors on runtime

**Files:**
- Modify: `internal/cli/runtime.go`

- [ ] **Step 1: Add `Locks()` and `Health()` methods on `*runtime`**

After the existing `Close()` method in `internal/cli/runtime.go`, add:

```go
// Locks returns the lock-ops view of the underlying store.
// Callers should prefer this over rt.Store when they only need lock ops.
func (r *runtime) Locks() store.LockOps { return r.Store }

// Health returns the audit/repair view of the underlying store.
func (r *runtime) Health() store.Health { return r.Store }
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/runtime.go
git commit -m "cli: add typed runtime accessors Locks() / Health() (loto-gp3)"
```

---

## Task 3: Switch cmd_lock to LockOps

**Files:**
- Modify: `internal/cli/cmd_lock.go:125`

- [ ] **Step 1: Replace the one call site**

In `internal/cli/cmd_lock.go`, change `rt.Store.AcquireLocks(...)` → `rt.Locks().AcquireLocks(...)`.

- [ ] **Step 2: Build + test**

Run: `go test ./internal/cli/... -run TestLock -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_lock.go
git commit -m "cli: cmd_lock uses LockOps (loto-gp3)"
```

---

## Task 4: Switch cmd_unlock to LockOps

**Files:**
- Modify: `internal/cli/cmd_unlock.go:60,83,120,140`

- [ ] **Step 1: Replace all four call sites**

In `internal/cli/cmd_unlock.go`, replace each of:
- `rt.Store.ReleaseLocks` → `rt.Locks().ReleaseLocks` (two sites: lines 60 and 140)
- `rt.Store.BreakLocks` → `rt.Locks().BreakLocks` (line 83)
- `rt.Store.ListLocks` → `rt.Locks().ListLocks` (line 120)

- [ ] **Step 2: Build + test**

Run: `go test ./internal/cli/... -run TestUnlock -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_unlock.go
git commit -m "cli: cmd_unlock uses LockOps (loto-gp3)"
```

---

## Task 5: Switch cmd_doctor to LockOps + Health

**Files:**
- Modify: `internal/cli/cmd_doctor.go:77,110,116,120,127`

- [ ] **Step 1: Replace call sites**

In `internal/cli/cmd_doctor.go`:
- `rt.Store.DoctorAuditWith` → `rt.Health().DoctorAuditWith` (line 77)
- `rt.Store.DoctorRepair` → `rt.Health().DoctorRepair` (line 110)
- `rt.Store.RestoreOrphanMode` → `rt.Health().RestoreOrphanMode` (line 116)
- `rt.Store.ListLocks` → `rt.Locks().ListLocks` (line 120)
- `rt.Store.ScanOrphanModes` → `rt.Health().ScanOrphanModes` (line 127)

- [ ] **Step 2: Build + test**

Run: `go test ./internal/cli/... -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_doctor.go
git commit -m "cli: cmd_doctor uses LockOps + Health (loto-gp3)"
```

---

## Task 6: Switch cmd_check + cmd_status to LockOps

**Files:**
- Modify: `internal/cli/cmd_check.go:47`
- Modify: `internal/cli/cmd_status.go:44,88`

- [ ] **Step 1: Replace call sites**

Three sites total — all `rt.Store.ListLocks(...)` → `rt.Locks().ListLocks(...)`.

- [ ] **Step 2: Build + test**

Run: `go test ./internal/cli/... -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_check.go internal/cli/cmd_status.go
git commit -m "cli: cmd_check + cmd_status use LockOps (loto-gp3)"
```

---

## Task 7: Lock the new locks_*.go files

The five new files don't exist yet, so `loto lock` rejected them in setup. Now create empty stubs, lock them, then fill in.

- [ ] **Step 1: Stub files**

```bash
for f in locks_acquire.go locks_release.go locks_break.go locks_query.go; do
  printf 'package store\n' > internal/store/$f
done
```

- [ ] **Step 2: Lock them**

```bash
loto lock \
  internal/store/locks_acquire.go \
  internal/store/locks_release.go \
  internal/store/locks_break.go \
  internal/store/locks_query.go \
  -t "loto-gp3: split locks.go into per-concern files" \
  -ttl 4h
```

Expected: `✓ locked count=4`.

---

## Task 8: Move AcquireLocks family → locks_acquire.go

**Files:**
- Modify: `internal/store/locks.go` (remove)
- Modify: `internal/store/locks_acquire.go` (populate)

Functions to move (from `locks.go`):
- `AcquireLocks` (method)
- `stripAndHandleFailure` (method)
- `appendModeRestoreFailedEvent` (method)
- `insertAllLocks` (helper)
- `restoreAll` (helper)
- `validateAllFileTargets` (helper)
- `validateFileTarget` (helper)
- `collectAllBlockers` (helper)
- `stripAll` (helper)
- `rollbackStripped` (helper)
- `collectBlockers` (helper)
- `insertOrRefreshLock` (helper)

- [ ] **Step 1: Move the code**

Cut the listed functions from `locks.go`, paste into `locks_acquire.go`. Keep `package store` header. Add the imports the moved code needs (`context`, `database/sql`, `errors`, `fmt`, `os`, `path/filepath`, `sort`, `time`, `loto/internal/domain` — verify by attempting build).

- [ ] **Step 2: Build**

Run: `go build ./internal/store/...`
Expected: exit 0. Fix imports if `goimports` flags missing/unused.

- [ ] **Step 3: Test**

Run: `go test ./internal/store/... -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/store/locks.go internal/store/locks_acquire.go
git commit -m "store: extract AcquireLocks family to locks_acquire.go (loto-gp3)"
```

---

## Task 9: Move ReleaseLocks family → locks_release.go

Functions to move:
- `ReleaseLocks` (method)
- `classifyReleases` (helper)
- `restoreAndAuditReleases` (method)
- `loadOwnersTx` (helper)
- `deleteOwnedTx` (helper)
- `restoreAndAudit` (method)

- [ ] **Step 1: Move the code**

Cut from `locks.go`, paste into `locks_release.go`. Add imports.

- [ ] **Step 2: Build + test**

Run: `go test ./internal/store/... -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/store/locks.go internal/store/locks_release.go
git commit -m "store: extract ReleaseLocks family to locks_release.go (loto-gp3)"
```

---

## Task 10: Move BreakLocks family → locks_break.go

Functions to move:
- `BreakLock` (method)
- `BreakLocks` (method)
- `classifyBreaks` (helper)
- `restoreAndAuditBreaks` (method)
- `loadLocksByTargetTx` (helper)

- [ ] **Step 1: Move the code**

Cut from `locks.go`, paste into `locks_break.go`. Add imports.

- [ ] **Step 2: Build + test**

Run: `go test ./internal/store/... -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/store/locks.go internal/store/locks_break.go
git commit -m "store: extract BreakLocks family to locks_break.go (loto-gp3)"
```

---

## Task 11: Move ListLocks + LockAt → locks_query.go

Functions to move:
- `ListLocks` (method)
- `LockAt` (method)

- [ ] **Step 1: Move the code**

Cut from `locks.go`, paste into `locks_query.go`. Add imports.

- [ ] **Step 2: Verify what remains in locks.go**

`locks.go` should now hold only:
- `MultiConflictError` + its `Error()` method
- `ChmodFailureError` + its `Error()` method
- `ChmodFailure` (struct)
- `chmodRestoreErr` (struct, if defined here)
- `ReleaseResult` + `BreakResult` (result structs)
- `inClause`, `inClauseStrings` (helpers)
- `modeRestoreFailedEvent` (helper)
- `loadLocksTx` (helper)
- `scanLock` (helper)
- `reclaimStaleTx` (helper)

Run: `wc -l internal/store/locks*.go`
Expected: each `locks_*.go` is roughly 100–250 LOC; `locks.go` is roughly 150–200 LOC.

- [ ] **Step 3: Build + test**

Run: `go test ./internal/store/... -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/store/locks.go internal/store/locks_query.go
git commit -m "store: extract ListLocks + LockAt to locks_query.go (loto-gp3)"
```

---

## Task 12: Full audit + lint re-run

- [ ] **Step 1: `make check`**

Run: `make check`
Expected: exit 0 (vet + test + golangci-lint all green).

- [ ] **Step 2: Re-run the original review themes**

Run: `make report` (or whatever drives the review pipeline in this repo — `make ci-report` is the alternative). Grep the output for theme-1 findings:

```bash
make report 2>&1 | tee /tmp/loto-gp3-report.txt
grep -E 'arch|solid|change-smells|truthful-names' /tmp/loto-gp3-report.txt | grep -i 'store\|locks\.go'
```

Expected: prior theme-1 findings against `internal/store` and `locks.go` are gone or reduced. Capture the residual into the bead notes.

- [ ] **Step 3: Decide on phase 2**

If the god-object finding (arch/solid against `*Store` method count) still fires, file a follow-up bead "phase 2: split *Store into role structs sharing unexported core" and reference loto-gp3 as `replaces`-precursor. Do **not** extend this PR.

---

## Task 13: PR + release loto locks

- [ ] **Step 1: Push branch + open PR**

```bash
git push -u origin loto-gp3
gh pr create --title "store: ISP at consumer edge + intra-pkg locks.go split (loto-gp3)" --body "$(cat <<'EOF'
## Summary
- Add `LockOps` and `Health` role interfaces in `internal/store/api.go`; `*Store` satisfies both via static-assert.
- Update CLI to depend on the narrowest interface per command (cmd_lock/unlock/check/status → `LockOps`; cmd_doctor → `LockOps` + `Health`) through new `rt.Locks()` / `rt.Health()` accessors.
- Split `internal/store/locks.go` (706 LOC) intra-package into `locks_acquire.go`, `locks_release.go`, `locks_break.go`, `locks_query.go`; shared helpers + error types remain in `locks.go`.

Replaces loto-81o (wontfix-as-specified). No public method signatures change. Closes the change-smells + truthful-names theme-1 findings; arch/solid status re-evaluated in Task 12.

## Test plan
- [x] `make check` green
- [x] No `*Store` method signatures change (grep verified)
- [x] Each `locks_*.go` ≤ ~250 LOC
- [x] Review pipeline re-run captured in bead notes
EOF
)"
```

- [ ] **Step 2: Close the bead**

```bash
bd close loto-gp3 --reason "PR #<N>"
```

- [ ] **Step 3: Release loto locks**

```bash
loto unlock \
  internal/store/locks.go \
  internal/store/locks_acquire.go \
  internal/store/locks_release.go \
  internal/store/locks_break.go \
  internal/store/locks_query.go \
  internal/cli/cmd_lock.go \
  internal/cli/cmd_unlock.go \
  internal/cli/cmd_doctor.go \
  internal/cli/cmd_check.go \
  internal/cli/cmd_status.go \
  internal/cli/runtime.go
```

(`cmd_check.go` and `cmd_status.go` were touched by Task 6 but not in the original lock set — lock them now if needed, then unlock the whole set.)

---

## Self-review checklist

- ✓ Spec coverage: api.go (Task 1), runtime accessors (Task 2), all 5 cmd_*.go switches (Tasks 3–6), locks.go split (Tasks 7–11), verification (Task 12), PR (Task 13). EventLog interface deliberately deferred — documented in Task 1.
- ✓ Placeholders: none. Every step shows exact code/command.
- ✓ Type consistency: `LockOps`, `Health` used uniformly throughout; runtime accessors `Locks()`, `Health()` consistent; static-assert verifies `*Store` satisfies both.
- ✓ Risk: pure refactor, no behavior change. Regression net is existing tests, which run after each task.
