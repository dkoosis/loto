# loto-9k8 — Add batch APIs: BreakLocks, AppendEvents; tx-ify ReleaseLocks

## Direction

Close the n-plus-one cluster from review run a608d43c6832 (theme 2) by adding
batch primitives to Store and migrating in-scope callers. Out-of-scope sites
(DoctorRepair F4) get a follow-up bead.

Acceptance (from bead): `BreakLocks([]Target)` and `AppendEvents([]Event)` on
the relevant store packages; one tx per batch; per-item error reporting
preserved; n-plus-one re-run shows the 6 findings closed; existing single-item
callers either migrated or preserved as thin wrappers.

## Scope

In scope (matches my locked blast paths):

- F1 — `Store.ReleaseLocks`: single-tx, `WHERE target_canonical IN (…)` SELECT,
  batched DELETE (`internal/store/locks.go`)
- F3 — `cli.breakTargets` loop replaced with `Store.BreakLocks`
  (`internal/store/locks.go` + `internal/cli/cmd_unlock.go`)
- F5 — `stripAndHandleFailure` chmod-restore-failure loop → one `AppendEvents`
  call (`internal/store/locks.go`)
- F6 — `Store.AppendEvents([]Event) error` primitive (`internal/store/events.go`)

Out of scope, deferred:

- F4 — `DoctorRepair` post-commit audit loop (`internal/store/doctor.go`,
  not in my locked territory) → file follow-up bead
- Pre-existing `intrange` lint in `internal/identity/registry.go:127` → filed
  as **loto-n1h**

## API design

### `(*Store).AppendEvents(ctx, evs []Event) error`

Single transaction. INSERT all rows, rotate events once, commit. Empty slice
is a no-op returning nil. Event.ID filled in-place if empty (mirrors
AppendEvent). Wrapper: `AppendEvent` delegates to `AppendEvents`.

### `(*Store).BreakLocks(ctx, ts []Target, byAgent, force, reason, live) ([]BreakResult, error)`

```go
type BreakResult struct {
    Target domain.Target
    Err    error // nil on success; ErrNoLockAtTarget, AuthorizeBreak error, etc.
}
```

Single tx:
1. SELECT all rows `WHERE target_canonical IN (…)`.
2. For each input target in input order: missing → result Err=ErrNoLockAtTarget;
   else AuthorizeBreak; on success collect event + target-to-delete.
3. Batched `appendEventsTx`; DELETE grouped by `owner_uuid` (typically 1 owner;
   degenerate N-owners still beats N transactions).
4. Commit.
5. Post-commit: `restoreWrite` per successful target. Failures collected into
   one `AppendEvents` call.

Per-target errors do not abort the batch (mirrors existing BreakLock semantics).
`BreakLock(t)` becomes a thin wrapper around `BreakLocks([]Target{t})`.

### `(*Store).ReleaseLocks` (internal-only change, signature unchanged)

Replace `releaseOne` loop with:
1. One SELECT `owner_uuid, target_canonical FROM locks WHERE target_canonical IN (…)`.
2. Classify each input target in memory: NoLock / NotOwner / pending-delete.
3. One batched DELETE for owner-matched targets.
4. Commit, then post-commit chmod restore per-path (FS-bound, stays per-path).
5. Restore failures batched through one `AppendEvents` call.

`releaseOne` removed; no external callers (verified via grep).

## TDD plan

New tests in `internal/store/events_test.go`:
- `TestAppendEvents_BatchInsert`: insert N events; verify ListEvents returns N.
- `TestAppendEvents_EmptySlice_NoOp`: empty input returns nil, no rows.
- `TestAppendEvents_AssignsIDs`: empty Event.ID gets assigned in-place.

New tests in `internal/store/locks_test.go`:
- `TestBreakLocks_BatchedMultiTarget`: break 3 targets; events for each, rows
  deleted, per-target results in input order.
- `TestBreakLocks_MixedNoLockAndOwned`: 2 owned + 1 missing; missing reports
  ErrNoLockAtTarget; owned ones succeed.
- `TestReleaseLocks_BatchedMixedStates`: release 3 targets covering Unlocked,
  NotOwner, NoLock.

Existing tests (`TestReleaseLock`, `TestBreakLockStaleOnly`) keep passing
unchanged — proves wrapper compat.

## Files edited

| File | Why |
|------|-----|
| `internal/store/events.go` | Add `AppendEvents`, refactor `AppendEvent` to wrapper |
| `internal/store/events_test.go` | New batch tests |
| `internal/store/locks.go` | Add `BreakLocks`, `BreakResult`; tx-ify `ReleaseLocks`; drop `releaseOne`; batch restore-failure audit |
| `internal/store/locks_test.go` | New batch tests |
| `internal/cli/cmd_unlock.go` | `breakTargets` → single `BreakLocks` call |

## Risks

- **Per-owner DELETE in BreakLocks** — group successful targets by `OwnerUUID`,
  one DELETE per group. 1 owner typical; N owners degenerate-but-still-one-tx.
- **MultiConflictError / ChmodFailureError contracts** untouched.
- **AcquireLocks** path untouched.
- **render.EmitBreakResults** doesn't exist; `breakTargets` keeps inline render
  until a render-side bead exists.

## Acceptance gate

1. `make check` introduces zero new findings (pre-existing intrange in
   registry.go:127 is loto-n1h, out of scope).
2. New tests pass; existing tests pass.
3. `rg 'for .* := range .* \{' internal/store/locks.go internal/cli/cmd_unlock.go | rg '\.(BreakLock|releaseOne|AppendEvent)\('`
   returns zero hits for in-scope sites.
4. F4 (`DoctorRepair`) deferred to follow-up bead.
