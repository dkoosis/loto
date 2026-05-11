# Pass 1 — Plan Adherence + Persistence Rigor (Tasks 1–4)

Bead: loto-qqh.1 · 3 commits · `make audit` green.

## Part A — Plan Adherence

Task 1 (schema bump + wipe-on-mismatch) ✓ — sentinel error, `preExisted` via `os.Stat`+`Size()>0`, distinct stderr messages, `MoveCorruptAside` flow. Test asserts wipe + aside file presence.

Task 2 (chmod helpers) ✓ — `stripWrite` clears `0o222`, `restoreWrite` ORs `0o200`, ENOENT-tolerant via `errors.Is(err, fs.ErrNotExist)`, `chmodFn` indirection in place. Spec §"no stored mode" trade documented in comment.

Task 3 (op-flock helper) ✓ — `LOTO_FLOCK_TIMEOUT` default 30s, 50ms poll, one-shot notice after 250ms via `sync.Once`, `ErrFlockTimeout` sentinel, kernel cleanup on exit. Tests cover serialization, wait-notice, timeout. Minor deviation: uses Go 1.22+ `wg.Go` and `for i := range 3` (cleaner than plan's snippet — improvement, not drift).

Task 4 (`opFlockPath`) ✓ — `filepath.Join(filepath.Dir(s.dbPath), "lock-op.flock")` matches spec. `Store` carries `dbPath` + `stderr` + `SetStderr` injector.

Deviations: none material. All plan invariants satisfied.

## Part B — Persistence Findings

### F1 · internal/store/store.go:50 · P2
Mechanism: TOCTOU between `os.Stat` and `sql.Open`. If a brand-new file is created (e.g., by a concurrent `loto` invocation) between Stat and Open, `preExisted=false` skips the version check; that concurrent process may have only written page-zero and a stale `user_version`. Window is microseconds and contention is already serialized by the op-flock at higher layers — but the op-flock lives *next to* the DB, so Open happens before flock acquisition. The race exists but is reachable only via simultaneous first-ever `Open` on the same path.
Fix: defer to Task 5+ — the op-flock must wrap `Open` (or check `user_version` post-`sql.Open` unconditionally and treat `0` as new only when DB has no tables).

### F2 · internal/store/store.go:38 · P2
Mechanism: `MoveCorruptAside` renames `loto.db` first, then best-efforts `-wal`/`-shm`. If the main rename succeeds but `-wal` rename fails silently (ignored `_ =`), the next `openOnce` creates a fresh DB beside a stranded `*-wal` from the prior incarnation. SQLite may attach to the orphan WAL on open and resurrect rolled-back state, or fail to start. This pre-exists this bead but the new user-version wipe path now exercises it.
Fix: in `MoveCorruptAside`, if the sibling rename fails for a reason other than ENOENT, surface it. Capture as follow-up bead.

### F3 · internal/store/store.go:34 · P3
Mechanism: stderr messages in `Open` write to `os.Stderr` directly, bypassing the new `s.stderr` injector. Tests cannot capture these. Inconsequential because `Open` returns before the Store is constructed in the mismatch branch — there is no `s` to read from yet. Note only.
Fix: accept; if a test ever needs to assert these messages, thread a writer through `Open`.

### F4 · internal/store/flock.go:69 · P3
Mechanism: `time.Now().After(deadline)` is checked *after* a 50ms sleep would happen on the next iteration but *before* the sleep — so total wait can overshoot `limit` by up to one poll interval (50ms). The `TestOpFlock_TimeoutAborts` budget (500ms for 100ms limit) absorbs this. Acceptable.
Fix: none.

### F5 · internal/store/chmod.go:30 · P3
Mechanism: `restoreWrite` Stats then chmods — if file is unlinked between Stat and chmod, chmodFn returns ENOENT and is propagated rather than swallowed. Spec says "deleted-while-held is not an error". The Stat-side ENOENT is handled; the chmod-side ENOENT is not. Tiny window.
Fix: in `restoreWrite`, also swallow `fs.ErrNotExist` returned by `chmodFn`. Trivial.

### F6 · internal/store/flock.go:50 · P3
Mechanism: op-flock file at `0o600` is created with no parent-dir creation. If `filepath.Dir(opFlockPath)` does not exist (state dir not provisioned), `OpenFile` fails. Currently the dir is always the DB's parent — guaranteed to exist post-`Open`. Becomes a real concern only when callers in Task 5+ call `acquireOpFlock` before `Open` succeeds.
Fix: callers in Task 5+ must order `Open` → `acquireOpFlock(s.opFlockPath())`. Note for Task 5 plan.

No P0 / P1 findings. Tasks 1–4 land cleanly.

## Recenter

> Step back. Re-read `docs/NORTH_STAR.md` tiers 3–4 (file flock + chmod). Is this diff on-direction for the lockout primitive as the north-star defines it? Answer in one paragraph.
