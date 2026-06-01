# loto shared/exclusive lock modes + downgrade — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Bead:** loto-k5el.2 (parent epic loto-k5el; sibling loto-k5el.1 = TTL/liveness)

**Goal:** Give loto locks a `shared` (multi-reader) vs `exclusive` (sole-writer) mode so several agents can hold a read lease on the same target without false contention, and let a holder downgrade `exclusive → shared` in place — "conflicts as a negotiation, not a wall".

**Architecture:** Add a `mode` column to the `locks` record (the same row sibling .1 extends with TTL/liveness fields). Change the lock table's primary key from `target_canonical` alone to a composite `(target_canonical, owner_uuid)` so a target can carry multiple coexisting shared holders. Rewrite the single conflict predicate that currently means "any other live holder blocks" into a mode-aware predicate (shared+shared = OK, exclusive-vs-anything = conflict). Add a store-level in-place downgrade that flips `exclusive → shared` and restores the write bit without an unlock/relock. CLI gains a `--shared` flag on `loto lock` and a `loto downgrade` verb.

**Tech Stack:** Go, SQLite (`internal/store/loto.db`), existing `internal/store` + `internal/cli` + `internal/render` + `internal/domain` packages, existing render conventions (`.claude/rules/design.md`).

---

## ‡ PROCESS RULE — this work ships via PR, never direct-to-main

Per `.claude/rules/workflow.md`: **every change under `internal/store/*` or `internal/identity/registry.go` ships through a PR.** `go test -race` runs only on the self-hosted CI runners (linux + macos), never on local macOS. This plan touches `internal/store/*` heavily (schema, acquire, release, query, chmod) — so:

- Implement on a feature branch, push, open a PR. Do **not** merge to main locally.
- The merge-conflict-prone files are shared with sibling .1 (`schema.sql`, `store.go`, `records.go`, `staleness.go`, `locks_acquire.go`). Coordinate ordering with .1 — see "§ Reconciliation with loto-k5el.1" below.
- CI runners are serial (`mac-loto`, `trixi-loto`); a burst of merges backlogs ~15–20 min. That lag is not breakage.

---

## § Reconciliation with loto-k5el.1 (READ FIRST — open question for dk)

Sibling **loto-k5el.1** adds TTL/liveness fields to the **same** `locks` record this plan extends with `mode`. At the time of writing **.1 has no committed plan or schema** (`docs/superpowers/plans/` has no `k5el.1` file; the .1 worktree has no plan commit). So .1's exact column set is **undecided**.

**Assumption taken by this plan (flag for dk):**

1. **.1 does NOT change the lock table's primary key.** TTL/liveness are per-row scalar fields (`expires_at` already exists; .1 adds liveness handles like a session/boot id). This plan owns the PK change `target_canonical → (target_canonical, owner_uuid)`, because multi-holder is meaningless for TTL but mandatory for shared mode.
2. **Both .1 and .2 follow the existing additive-ALTER-without-version-bump migration precedent** (the `proc_start` upgrade in `store.go::ensureLocksProcStart`, loto-kwlp). New columns are added in-place; `schemaUserVersion` is **not** bumped, because a bump trips `MoveCorruptAside` and destroys live locks. Both siblings add their columns this way; neither bumps the version.
3. **.1's IsStale predicate and .2's conflict predicate compose cleanly.** Today's acquire path already filters stale holders *before* collecting blockers (`reclaimStaleAndCollectBlockers`). This plan inserts the mode check at the blocker-collection step, *after* the stale filter. So a stale shared lock is reclaimed (per .1) and never reaches the mode check (per .2). No ordering conflict — but if .1 restructures `reclaimStaleAndCollectBlockers`, the two changes touch the same function and must be merged by hand.

**RECONCILIATION POINT FOR dk:** If .1's final schema *does* change the PK, or *does* restructure the blocker-collection loop, tasks 1 and 3 below need a rebase. Whichever sibling lands second rebases onto the first. Recommend .1 (liveness, the higher-value piece per its bead) lands first; .2 rebases its PK change and conflict predicate on top.

---

## Formal model (claudish)

```
LockMode = shared | exclusive

LockRecord = {target, owner, session, intent, created_at, expires_at,
              host, pid, proc_start, branch, mode}          -- mode is new

-- Holders on a target after stale-reclaim:
Holders(t) = { l ∈ locks : l.target = t ∧ ¬IsStale(l) }     -- IsStale from .1/today

-- Conflict predicate (the heart of this bead).
-- An incoming acquire `a` (mode m_a, owner o_a) conflicts with existing holder `l`:
Conflicts(a, l) ≡ l.owner ≠ a.owner
                ∧ SameCanonical(a.target, l.target)
                ∧ ¬IsStale(l)
                ∧ (a.mode = exclusive ∨ l.mode = exclusive)
        -- i.e. shared+shared = NO conflict; any exclusive on either side = conflict

Blockers(a) = { l ∈ locks : Conflicts(a, l) }
AcquireOK(a) ≡ Blockers(a) = ∅

-- Same-owner re-acquire is an upsert (today's ON CONFLICT DO UPDATE), now keyed
-- on (target, owner). A same-owner re-acquire may change mode → that is a
-- downgrade/upgrade in disguise; see Downgrade below.

Downgrade(o, t): exclusive → shared, in place
  pre:  ∃ l ∈ locks : l.target=t ∧ l.owner=o ∧ l.mode=exclusive
  post: that row's mode = shared ∧ write-bit restored on t
        ∧ no unlock event, no relock; emits lock_downgraded event
  -- Upgrade shared→exclusive is OUT OF SCOPE (non-goal; would require
  -- re-stripping the write bit + a fresh conflict check against peer shared holders).
```

**Invariants:**

```
I1: PRIMARY KEY (target_canonical, owner_uuid)  -- ≤1 row per (target, owner)
I2: many shared holders may coexist on one target; ≤1 exclusive holder, and an
    exclusive holder excludes all others (shared or exclusive)
I3: legacy rows (no mode column value) read as `exclusive` — preserves today's
    "binary lock = sole writer" semantics; no behavior change for existing locks
I4: write-bit is stripped (read-only) iff an exclusive holder exists on the target.
    A shared-only target keeps its write bit. (See §Write-bit semantics.)
I5: downgrade is monotonic in this plan: exclusive→shared only, never the reverse
I6: mode column added via additive ALTER; schemaUserVersion NOT bumped (loto-kwlp precedent)
```

---

## § Write-bit semantics (design decision — surfaced, not silently chosen)

Today `AcquireLocks` **strips the owner-write bit** off every locked file (`stripAll` → `stripWrite`), making it read-only on disk; `ReleaseLocks`/downgrade restore it. This is loto's teeth — an agent that ignores the advisory lock still hits a read-only file.

For **shared** locks this is wrong: multiple readers don't need the file read-only, and a reader stripping the write bit would surprise the (legitimate) exclusive-writer-to-be. **Decision for this plan:**

- **exclusive acquire** → strip write bit (today's behavior, unchanged).
- **shared acquire** → do **NOT** strip the write bit. A shared lock is purely advisory; it records "I'm reading this" and coexists with other readers. The write bit is a property of the *target*, owned by the (at most one) exclusive holder.
- **downgrade exclusive→shared** → restore the write bit (the last exclusive hold is gone).
- **release of a shared lock** → no write-bit change (it was never stripped by us).
- **edge:** exclusive holder downgrades to shared while shared peers exist → write bit restored, peers unaffected (they never relied on it).

‡ This keeps the chmod logic keyed on **exclusive mode only**, which is the minimal, conservative change. `stripAll` and the release-restore path both gain a `mode == exclusive` guard.

---

## § check --staged interaction (THE EPIC'S OPEN QUESTION — surfaced for dk, not decided here)

The epic (loto-k5el) and bead .2 both flag this explicitly:

> check --staged: keep hard-block, or move to grant-with-warning (wait/narrow/downgrade)? Protocol doc currently favors hard refuse.

`loto check --staged` is the **machine surface** the trixi pre-commit/PreToolUse guard parses (`cmd_check.go::appendCheckConflictsForTarget`). Its current predicate: any non-self, non-stale holder on a staged path = a hard conflict (exit 1).

**This plan does the minimal correct thing and stops at the policy line:**

- `check` adopts the **same mode-aware conflict predicate** as acquire (a shared peer reading a file you're committing is not a conflict; an exclusive holder is). This is a pure correctness fix — a shared peer was never a real blocker.
- `check` continues to **hard-block (exit 1) on a genuine conflict** (exclusive holder on a staged path). It does **NOT** move to grant-with-warning. The "conflicts as negotiation" softening for the pre-commit gate is deliberately **left as a dk decision** — the epic says the protocol "currently favors hard refuse for safety," and silently relaxing the commit gate is exactly the kind of safety change that needs a human call.

**RECONCILIATION POINT FOR dk:** Do you want `check --staged` to (a) stay a hard block on exclusive conflicts [this plan's choice], or (b) move to grant-with-warning so a committing agent is told "exclusive holder present — wait / ask them to downgrade" but is not blocked? Option (b) is a one-line change in `cmd_check.go` (exit 0 + a `⚠` line instead of exit 1) but changes a safety-critical gate. Flagged, not taken.

---

## File structure

```
internal/domain/
  records.go        MODIFY  — add LockRecord.Mode field (string) + LockMode consts + EffectiveMode helper
  staleness.go      MODIFY  — add Conflicts(a, l) mode-aware predicate (method on EvalContext)
  records_test.go   (new test file or extend) — Mode default + Conflicts truth table

internal/store/
  schema.sql        MODIFY  — locks PK → (target_canonical, owner_uuid); add `mode TEXT NOT NULL DEFAULT 'exclusive'`; widen events CHECK
  store.go          MODIFY  — ensureLocksModeAndPK() additive rebuild (mirrors ensureLocksProcStart); call in migrate(); extend schemaFullyCurrent probe
  locks.go          MODIFY  — lockCols gains `mode`; scanLock reads it (NULL/'' → exclusive); add EventLockDowngraded const
  locks_acquire.go  MODIFY  — insertOrRefreshLock ON CONFLICT key → (target,owner) + mode col; blocker collection uses Conflicts; stripAll guarded by mode==exclusive
  locks_query.go    MODIFY  — add LockForOwnerAt (multi-holder-safe lookup)
  locks_release.go  MODIFY  — release of shared lock skips write-bit restore (carry mode in ReleaseResult)
  locks_downgrade.go CREATE — DowngradeLock(ctx, target, owner): exclusive→shared in tx + restore write bit + lock_downgraded event
  locks_downgrade_test.go CREATE
  migrate_mode_test.go CREATE — mode column present, composite PK, lock_downgraded event allowed

internal/cli/
  cmd_lock.go       MODIFY  — add `--shared` flag (default exclusive); thread mode into buildLockRecords
  cmd_downgrade.go  CREATE  — `loto downgrade <target>...` verb
  cmd_downgrade_test.go CREATE
  cmd_check.go      MODIFY  — appendCheckConflictsForTarget uses Conflicts (shared peer ≠ conflict)
  cmd_status.go     MODIFY  — show mode per lock row
  cli.go            MODIFY  — register `downgrade` (if not init-based)

internal/render/
  cli.go            MODIFY  — lock-success + conflict + status rows show mode=shared|exclusive
  cli_test.go       MODIFY  — assert mode appears, deterministic

docs/
  NORTH_STAR.md     MODIFY  — append §Lock modes subsection (model, I1–I6, write-bit rule, check policy)
```

---

## Schema delta

```sql
-- schema.sql — locks table becomes:
CREATE TABLE IF NOT EXISTS locks (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  -- mode: 'shared' (multi-reader, write-bit NOT stripped) or 'exclusive'
  -- (sole-writer, write-bit stripped). Legacy rows / NULL read as 'exclusive'
  -- to preserve today's binary-lock semantics (loto-k5el.2). Added in-place via
  -- the guarded rebuild in migrate(); declared here so fresh DBs match.
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  PRIMARY KEY (target_canonical, owner_uuid)
);
CREATE INDEX IF NOT EXISTS idx_locks_target   ON locks(target_canonical);
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);
```

‡ **The PK change is NOT achievable by `ALTER TABLE` in SQLite.** SQLite cannot add/drop a primary key in place. For **fresh DBs** the new `CREATE TABLE` above is authoritative. For **existing DBs** carrying the old single-column PK, the additive-ALTER pattern can add the `mode` *column* but cannot change the PK. Decision (Task 1): the PK migration for existing DBs uses the **12-step table-rebuild** (`CREATE locks_new … ; INSERT INTO locks_new SELECT …, 'exclusive' ; DROP locks ; ALTER RENAME`) inside the migrate tx, guarded by a probe that checks whether the PK is already composite. This is heavier than the `proc_start` precedent — see Task 1 for the exact guard and why it does NOT bump `schemaUserVersion`.

```sql
-- events table CHECK constraint gains the new kind:
event_kind TEXT NOT NULL CHECK (event_kind IN (
  'lock_acquired','lock_released','lock_broken','lock_reclaimed_stale',
  'mode_restore_failed','acquire_rollback_started','lock_downgraded'))
```

‡ Changing a CHECK constraint also can't be done by ALTER — but the events `CREATE TABLE IF NOT EXISTS` only fires on a fresh DB. For existing DBs the old CHECK lacks `lock_downgraded`, so an INSERT of that kind would fail. Task 6 handles this with the same rebuild-or-fresh logic.

---

## Tasks

### Task 1: schema — composite PK + mode column + migration

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go` (add `ensureLocksModeAndPK` + composite-PK rebuild; call from `migrate`; extend `schemaFullyCurrent`)
- Test: a new `internal/store/migrate_mode_test.go`

- [ ] **Step 1: Write the failing migration test**

```go
// internal/store/migrate_mode_test.go
package store

import (
	"context"
	"testing"
)

func TestMigrate_AddsModeColumn(t *testing.T) {
	s := openTestStore(t) // existing helper; opens a fresh DB through Open()
	ctx := context.Background()
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'mode'`).Scan(&n)
	if err != nil {
		t.Fatalf("probe mode column: %v", err)
	}
	if n != 1 {
		t.Fatalf("want mode column present, got count=%d", n)
	}
}

func TestMigrate_LocksPKIsComposite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// pragma_table_info.pk is the 1-based position in the PK, 0 if not part of it.
	var pkCols int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		t.Fatalf("probe pk: %v", err)
	}
	if pkCols != 2 {
		t.Fatalf("want composite PK over 2 columns, got %d", pkCols)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestMigrate_AddsModeColumn|TestMigrate_LocksPKIsComposite' -v`
Expected: FAIL — `mode` column absent, `pkCols == 1`.

- [ ] **Step 3: Update schema.sql**

Replace the `CREATE TABLE IF NOT EXISTS locks (...)` block with the composite-PK + `mode`-column version from the "Schema delta" section above. Add `CREATE INDEX IF NOT EXISTS idx_locks_target ON locks(target_canonical);`. Widen the events `event_kind` CHECK to include `'lock_downgraded'` (per Schema delta).

- [ ] **Step 4: Add the in-place upgrade for existing DBs in store.go**

The `proc_start` precedent (`ensureLocksProcStart`) adds a column without bumping the version. The PK change additionally needs a table rebuild. Add it, guarded, inside the existing `migrate` tx (after `ensureLocksProcStart`):

```go
// internal/store/store.go — call site inside migrate(), right after the
// existing ensureLocksProcStart(ctx, tx) call:
	if err := ensureLocksProcStart(ctx, tx); err != nil {
		return fmt.Errorf("add locks.proc_start: %w", err)
	}
	if err := ensureLocksModeAndPK(ctx, tx); err != nil {
		return fmt.Errorf("upgrade locks mode/pk: %w", err)
	}
```

```go
// ensureLocksModeAndPK brings a pre-mode DB up to the composite-PK + mode-column
// shape. SQLite cannot ALTER a primary key in place, so when the PK is still the
// legacy single column we rebuild the table (12-step idiom) inside the migrate
// tx, defaulting every existing row's mode to 'exclusive' (preserving today's
// binary-lock = sole-writer semantics). user_version is intentionally NOT bumped
// — a bump trips MoveCorruptAside and destroys live locks (loto-kwlp precedent).
// Guarded by a PK-shape probe so this is a no-op on fresh DBs (CREATE TABLE
// already declared the composite PK) and on every re-Open.
func ensureLocksModeAndPK(ctx context.Context, tx *sql.Tx) error {
	var pkCols int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		return err
	}
	if pkCols == 2 {
		return nil // already migrated (fresh DB or prior upgrade)
	}
	// Legacy single-column PK: rebuild. The old table has no `mode` column, so
	// SELECT supplies the literal 'exclusive' for it.
	const rebuild = `
CREATE TABLE locks_new (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  PRIMARY KEY (target_canonical, owner_uuid)
);
INSERT INTO locks_new
  (target_canonical, owner_uuid, session_uuid, intent, created_at,
   expires_at, host, pid, proc_start, branch, mode)
SELECT target_canonical, owner_uuid, session_uuid, intent, created_at,
       expires_at, host, pid, proc_start, branch, 'exclusive'
FROM locks;
DROP TABLE locks;
ALTER TABLE locks_new RENAME TO locks;
CREATE INDEX IF NOT EXISTS idx_locks_target   ON locks(target_canonical);
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);`
	_, err := tx.ExecContext(ctx, rebuild)
	return err
}
```

‡ The pre-mode legacy table already had `owner_uuid` as a plain column (it's in today's schema), so the SELECT column list is valid. The rebuild runs inside the migrate tx → atomic; a crash rolls back to the old table.

- [ ] **Step 4b: Extend schemaFullyCurrent's probe**

`schemaFullyCurrent` (store.go) gates the no-write migrate fast path. It currently probes only `proc_start`. Without a `mode`/PK probe a stale DB at the current `user_version` would skip the rebuild. Fold the new conditions into its return:

```go
// In schemaFullyCurrent (store.go) — AND the existing proc_start probe with:
	var modeN int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'mode'`).Scan(&modeN); err != nil {
		return false // probe failure → treat as not-current (run migrate)
	}
	var pkCols int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		return false
	}
	// require BOTH the existing proc_start condition AND mode present AND composite PK
	return procStartPresent && modeN == 1 && pkCols == 2
```

(Adapt to the exact existing return expression — the existing function returns the proc_start probe result; fold the new conditions into that boolean.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestMigrate_AddsModeColumn|TestMigrate_LocksPKIsComposite' -v`
Expected: PASS. Then `go test ./internal/store/...` — existing tests should still pass (legacy rows default to `exclusive`, behavior unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/migrate_mode_test.go
git commit -m "feat(store): locks composite PK + mode column, in-place rebuild (loto-k5el.2 T1)"
```

---

### Task 2: domain — Mode field + Conflicts predicate

**Files:**
- Modify: `internal/domain/records.go`
- Modify: `internal/domain/staleness.go`
- Test: `internal/domain/records_test.go` (create or extend)

- [ ] **Step 1: Write the failing predicate test (truth table)**

```go
// internal/domain/records_test.go
package domain

import (
	"testing"
	"time"
)

func mk(owner, mode string) LockRecord {
	return LockRecord{
		Target:    Target{Canonical: "/a.go"},
		OwnerUUID: owner,
		Mode:      mode,
		ExpiresAt: time.Now().Add(time.Hour), // not stale
		PID:       0,                          // PID<=0 → never instant-stale
	}
}

func TestConflicts_TruthTable(t *testing.T) {
	ec := EvalContext{Now: time.Now()}
	cases := []struct {
		name string
		a, l LockRecord
		want bool
	}{
		{"shared+shared diff owner", mk("alice", ModeShared), mk("bob", ModeShared), false},
		{"shared+excl   diff owner", mk("alice", ModeShared), mk("bob", ModeExclusive), true},
		{"excl+shared   diff owner", mk("alice", ModeExclusive), mk("bob", ModeShared), true},
		{"excl+excl     diff owner", mk("alice", ModeExclusive), mk("bob", ModeExclusive), true},
		{"same owner never conflicts", mk("alice", ModeExclusive), mk("alice", ModeExclusive), false},
		{"empty mode reads as exclusive", mk("alice", ""), mk("bob", ModeShared), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ec.Conflicts(c.a, c.l); got != c.want {
				t.Fatalf("Conflicts(%s,%s) = %v, want %v", c.a.Mode, c.l.Mode, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/ -run TestConflicts_TruthTable -v`
Expected: FAIL — `Mode`, `ModeShared`, `ModeExclusive`, `Conflicts` undefined.

- [ ] **Step 3: Add the Mode field and consts**

```go
// internal/domain/records.go — add to LockRecord struct (after Branch):
	// Mode is the lease mode: ModeShared (multi-reader, advisory only, write-bit
	// NOT stripped) or ModeExclusive (sole-writer, write-bit stripped). Empty
	// string reads as exclusive — preserves the pre-mode binary-lock semantics
	// for legacy rows (loto-k5el.2). Normalize via EffectiveMode().
	Mode string
```

```go
// internal/domain/records.go — add consts + helper:
const (
	ModeShared    = "shared"
	ModeExclusive = "exclusive"
)

// EffectiveMode normalizes a possibly-empty Mode to exclusive (legacy default).
func (l LockRecord) EffectiveMode() string {
	if l.Mode == ModeShared {
		return ModeShared
	}
	return ModeExclusive // empty or any non-"shared" value → exclusive
}
```

- [ ] **Step 4: Add the Conflicts predicate**

```go
// internal/domain/staleness.go — add:

// Conflicts reports whether an incoming acquire `a` is blocked by existing
// holder `l`. Shared+shared on the same target coexist; an exclusive lease on
// either side conflicts. Same-owner holders never conflict (re-acquire is an
// upsert). A stale holder never conflicts — the caller is expected to have
// reclaimed it, but this guards the predicate independently (loto-k5el.2).
func (c EvalContext) Conflicts(a, l LockRecord) bool {
	if l.OwnerUUID == a.OwnerUUID {
		return false
	}
	if !SameCanonical(a.Target, l.Target) {
		return false
	}
	if c.IsStale(l) {
		return false
	}
	return a.EffectiveMode() == ModeExclusive || l.EffectiveMode() == ModeExclusive
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/domain/ -run TestConflicts_TruthTable -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/records.go internal/domain/staleness.go internal/domain/records_test.go
git commit -m "feat(domain): LockRecord.Mode + mode-aware Conflicts predicate (loto-k5el.2 T2)"
```

---

### Task 3: store — wire mode through scan/insert + mode-aware blocker collection

**Files:**
- Modify: `internal/store/locks.go` (`lockCols`, `scanLock`)
- Modify: `internal/store/locks_acquire.go` (`insertOrRefreshLock`, `reclaimStaleAndCollectBlockers`)
- Test: a new `internal/store/locks_shared_test.go`

- [ ] **Step 1: Write the failing store-level test**

```go
// internal/store/locks_shared_test.go (package store, mirror existing test pkg decl)

// Uses existing helpers (openTestStore). mustLockRecord is a small local helper
// that builds a domain.LockRecord with the given mode, a real temp file as the
// target, OwnerUUID set, and PID 0 (non-durable → TTL governs liveness).
// deadProbe is a domain.PidLiveProbe returning false. Mirror locks_test.go.

func TestAcquire_SharedSharedCoexist(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := mustLockRecord(t, "/a.go", aliceUUID, domain.ModeShared)
	b := mustLockRecord(t, "/a.go", bobUUID, domain.ModeShared)

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, deadProbe); err != nil {
		t.Fatalf("alice shared acquire: %v", err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, deadProbe); err != nil {
		t.Fatalf("bob shared acquire should succeed (shared+shared): %v", err)
	}
	rows, _ := s.ListLocks(ctx)
	if len(rows) != 2 {
		t.Fatalf("want 2 coexisting shared rows, got %d", len(rows))
	}
}

func TestAcquire_ExclusiveBlocksShared(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := mustLockRecord(t, "/a.go", aliceUUID, domain.ModeExclusive)
	b := mustLockRecord(t, "/a.go", bobUUID, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a}, deadProbe); err != nil {
		t.Fatalf("alice exclusive: %v", err)
	}
	_, err := s.AcquireLocks(ctx, []domain.LockRecord{b}, deadProbe)
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("want MultiConflictError (exclusive blocks shared), got %v", err)
	}
}
```

(Note: real targets must pass `validateFileTarget` (Lstat regular-file). Use `tmpFile(t)` paths, not the literal `/a.go`, in the actual test — the literals above are illustrative.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestAcquire_SharedSharedCoexist|TestAcquire_ExclusiveBlocksShared' -v`
Expected: FAIL — shared+shared currently blocks (binary predicate), and the old PK upsert collapses the two rows.

- [ ] **Step 3: Add mode to lockCols and scanLock**

```go
// internal/store/locks.go
const lockCols = `target_canonical,owner_uuid,session_uuid,intent,created_at,expires_at,host,pid,proc_start,branch,mode`
```

```go
// scanLock — add mode scanning. mode is NOT NULL DEFAULT 'exclusive' in fresh
// schema, but a sql.NullString keeps it robust against any NULL legacy row.
	var mode sql.NullString
	if err := r.Scan(&canonical, &l.OwnerUUID, &l.SessionUUID, &l.Intent,
		&createdNs, &expiresNs, &l.Host, &l.PID, &procStart, &l.Branch, &mode); err != nil {
		return l, err
	}
	...
	if mode.Valid {
		l.Mode = mode.String
	}
	// l.Mode == "" falls through; EffectiveMode() treats it as exclusive.
```

- [ ] **Step 4: Update insertOrRefreshLock — composite key + mode column**

```go
// internal/store/locks_acquire.go — insertOrRefreshLock
	_, err := tx.ExecContext(ctx, `
INSERT INTO locks(target_canonical, owner_uuid, session_uuid, intent, created_at, expires_at, host, pid, proc_start, branch, mode)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(target_canonical, owner_uuid) DO UPDATE SET
  intent=excluded.intent,
  expires_at=excluded.expires_at,
  session_uuid=excluded.session_uuid,
  host=excluded.host,
  pid=excluded.pid,
  proc_start=excluded.proc_start,
  branch=excluded.branch,
  mode=excluded.mode`,
		l.Target.Canonical, l.OwnerUUID, l.SessionUUID,
		l.Intent, l.CreatedAt.UnixNano(), l.ExpiresAt.UnixNano(),
		l.Host, l.PID, procStart, l.Branch, l.EffectiveMode(),
	)
```

‡ The `ON CONFLICT` target changes from `(target_canonical)` to `(target_canonical, owner_uuid)`. The old `WHERE locks.owner_uuid = excluded.owner_uuid` guard is now **redundant** (the conflict is already keyed on owner) — remove it. Persisting `EffectiveMode()` (not raw `l.Mode`) ensures the column never stores `''`.

- [ ] **Step 5: Make blocker collection mode-aware**

```go
// internal/store/locks_acquire.go — reclaimStaleAndCollectBlockers
// Currently the inner loop is:
//   if !domain.SameCanonical(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID { continue }
//   if ec.IsStale(*ex) { reclaim; continue }
//   blockers = append(blockers, all[i])
// becomes:
	for i := range all {
		ex := &all[i]
		if !domain.SameCanonical(ex.Target, l.Target) || ex.OwnerUUID == l.OwnerUUID {
			continue
		}
		if ec.IsStale(*ex) {
			if err := reclaimStaleTx(ctx, tx, *ex, l.OwnerUUID, ec.Now); err != nil {
				return nil, err
			}
			continue
		}
		// Mode-aware: a shared peer does not block a shared acquire (loto-k5el.2).
		if ec.Conflicts(l, *ex) {
			blockers = append(blockers, all[i])
		}
	}
```

‡ The same-canonical / same-owner / stale guards above are kept for the reclaim side-effect (stale locks must still be reaped, not just skipped). `ec.Conflicts(l, *ex)` is the final gate on whether a live, non-self peer actually blocks. (It re-checks those conditions internally; the duplication is intentional — the loop needs the reclaim branch, Conflicts needs to be self-contained for the check path in Task 8.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestAcquire_SharedSharedCoexist|TestAcquire_ExclusiveBlocksShared' -v`
Expected: PASS. Then `go test ./internal/store/...`.

- [ ] **Step 7: Commit**

```bash
git add internal/store/locks.go internal/store/locks_acquire.go internal/store/locks_shared_test.go
git commit -m "feat(store): mode-aware blocker predicate + composite-key upsert (loto-k5el.2 T3)"
```

---

### Task 4: store — write-bit stripped only for exclusive

**Files:**
- Modify: `internal/store/locks_acquire.go` (`stripAll`)
- Modify: `internal/store/locks_release.go` (`loadOwnersTx`, `classifyReleases`, `restoreAndAuditReleases`, `ReleaseBySession`)
- Modify: `internal/store/locks.go` (`ReleaseResult` gains `Mode string`)
- Test: `internal/store/locks_shared_test.go` (extend)

- [ ] **Step 1: Write the failing test**

```go
func TestAcquire_SharedDoesNotStripWriteBit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := mustLockRecord(t, tmpFile(t), aliceUUID, domain.ModeShared)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, deadProbe); err != nil {
		t.Fatalf("shared acquire: %v", err)
	}
	fi, _ := os.Stat(rec.Target.Canonical)
	if fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("shared lock must NOT strip owner-write bit; perm=%v", fi.Mode().Perm())
	}
}

func TestAcquire_ExclusiveStripsWriteBit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := mustLockRecord(t, tmpFile(t), aliceUUID, domain.ModeExclusive)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, deadProbe); err != nil {
		t.Fatalf("exclusive acquire: %v", err)
	}
	fi, _ := os.Stat(rec.Target.Canonical)
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("exclusive lock must strip owner-write bit; perm=%v", fi.Mode().Perm())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestAcquire_SharedDoesNotStripWriteBit' -v`
Expected: FAIL — `stripAll` currently strips every target unconditionally.

- [ ] **Step 3: Guard stripAll on mode==exclusive**

```go
// internal/store/locks_acquire.go — stripAll
func stripAll(sorted []domain.LockRecord) ([]string, *ChmodFailure) {
	stripped := make([]string, 0, len(sorted))
	for i := range sorted {
		if sorted[i].EffectiveMode() != domain.ModeExclusive {
			continue // shared locks are advisory-only; write bit untouched
		}
		p := sorted[i].Target.Canonical
		if err := stripWrite(p); err != nil {
			return stripped, &ChmodFailure{Target: sorted[i].Target, Err: err}
		}
		stripped = append(stripped, p)
	}
	return stripped, nil
}
```

‡ `stripped` (the returned slice) is what the rollback/restore paths operate on. Because shared targets are never added to it, the existing `restoreAllAndAudit` rollback logic already does the right thing — it only restores what was stripped.

- [ ] **Step 4: Carry mode into the release path and guard restore**

`restoreAndAuditReleases` (locks_release.go) currently calls `restoreWrite` on every successfully-unlocked target. A shared lock never stripped the bit, so restoring it would spuriously *add* owner-write to a file the agent may not own write on. Restore only when the released lock was exclusive.

```go
// internal/store/locks.go — add Mode to ReleaseResult:
type ReleaseResult struct {
	Target     domain.Target
	State      ReleaseOutcome
	Holder     string
	Mode       string // populated from the released row; "" → exclusive
	RestoreErr error
	AuditErr   error
}
```

```go
// internal/store/locks_release.go — loadOwnersTx: select mode too.
type ownerMode struct{ Owner, Mode string }

// in loadOwnersTx, change query + map value type to ownerMode:
	rows, err := tx.QueryContext(ctx,
		`SELECT target_canonical, owner_uuid, mode FROM locks WHERE target_canonical IN (`+placeholders+`)`, args...) //nolint:gosec
	...
	var canonical, owner, mode string
	if err := rows.Scan(&canonical, &owner, &mode); err != nil { return nil, err }
	out[canonical] = ownerMode{Owner: owner, Mode: mode}
```

```go
// classifyReleases — set results[i].Mode for owned rows:
	case o.Owner == byAgent:  // adjust to ownerMode field access
		results[i].State = StateUnlocked
		results[i].Mode  = o.Mode
		owned = append(owned, t.Canonical)
```

```go
// restoreAndAuditReleases — skip restore for shared releases:
	for i := range results {
		if results[i].State != StateUnlocked {
			continue
		}
		if domain.LockRecord{Mode: results[i].Mode}.EffectiveMode() == domain.ModeShared {
			continue // never stripped, nothing to restore
		}
		if rerr := restoreWrite(results[i].Target.Canonical); rerr != nil {
			... // unchanged
		}
	}
```

‡ `ReleaseBySession` builds its results without a mode lookup. Extend `loadSessionTargetsTx` to also select `mode`, return `[]struct{Canonical, Mode string}`, and populate `ReleaseResult.Mode` so session-scoped release applies the same guard.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'Strip|Release' -v` then `go test ./internal/store/...`.
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/locks.go internal/store/locks_acquire.go internal/store/locks_release.go internal/store/locks_shared_test.go
git commit -m "feat(store): strip/restore write bit only for exclusive locks (loto-k5el.2 T4)"
```

---

### Task 5: store — DowngradeLock (exclusive → shared, in place)

**Files:**
- Create: `internal/store/locks_downgrade.go`
- Modify: `internal/store/locks.go` (add `EventLockDowngraded` const)
- Modify: `internal/store/locks_query.go` (add `LockForOwnerAt`)
- Test: `internal/store/locks_downgrade_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/store/locks_downgrade_test.go
func TestDowngrade_ExclusiveToShared_RestoresWriteBit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	f := tmpFile(t)
	rec := mustLockRecord(t, f, aliceUUID, domain.ModeExclusive)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, deadProbe); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected stripped before downgrade")
	}
	if err := s.DowngradeLock(ctx, domain.Target{Canonical: f}, aliceUUID); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	l, _ := s.LockForOwnerAt(ctx, domain.Target{Canonical: f}, aliceUUID)
	if l == nil || l.EffectiveMode() != domain.ModeShared {
		t.Fatalf("want shared after downgrade, got %v", l)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("downgrade must restore owner-write; perm=%v", fi.Mode().Perm())
	}
}

func TestDowngrade_NoLock_Errors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	f := tmpFile(t)
	err := s.DowngradeLock(ctx, domain.Target{Canonical: f}, aliceUUID)
	if !errors.Is(err, ErrNoLockAtTarget) {
		t.Fatalf("want ErrNoLockAtTarget, got %v", err)
	}
}

func TestDowngrade_AlreadyShared_NoOp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	f := tmpFile(t)
	rec := mustLockRecord(t, f, aliceUUID, domain.ModeShared)
	_, _ = s.AcquireLocks(ctx, []domain.LockRecord{rec}, deadProbe)
	if err := s.DowngradeLock(ctx, domain.Target{Canonical: f}, aliceUUID); err != nil {
		t.Fatalf("downgrade of already-shared should be a no-op, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestDowngrade -v`
Expected: FAIL — `DowngradeLock`, `LockForOwnerAt` undefined.

- [ ] **Step 3: Add LockForOwnerAt query (multi-holder safe lookup)**

```go
// internal/store/locks_query.go
// LockForOwnerAt returns the single lock at target held by owner, or (nil,nil)
// if none. Replaces LockAt for the multi-holder world: LockAt assumed one row
// per target and is now ambiguous on a shared target with several holders.
func (s *Store) LockForOwnerAt(ctx context.Context, t domain.Target, owner string) (*domain.LockRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+lockCols+` FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		t.Canonical, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil //nolint:nilnil // (nil,nil) = no row
	}
	l, err := scanLock(rows)
	if err != nil {
		return nil, err
	}
	return &l, rows.Err()
}
```

‡ Audit callers of the existing `LockAt` (singular). Any that assumed one-row-per-target must move to `LockForOwnerAt` or a plural lister. Grep: `rg -n 'LockAt\(' internal/`. Likely callers: `cmd_status.go`, tag lookup. Fix each in the task that owns its file (status → Task 8). Keep `LockAt` only if a caller genuinely wants "any holder" semantics; otherwise deprecate.

- [ ] **Step 4: Implement DowngradeLock**

```go
// internal/store/locks_downgrade.go
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"loto/internal/domain"
)

// DowngradeLock flips an exclusive lock held by owner on target to shared,
// in place, and restores the owner-write bit — no unlock/relock, no new
// created_at (the hold is continuous). A lock that is already shared is a
// no-op. No lock at all returns ErrNoLockAtTarget. Emits a lock_downgraded
// audit event. The write-bit restore happens AFTER commit (mirrors release):
// the row state is authoritative; a restore failure is audited, not rolled back.
func (s *Store) DowngradeLock(ctx context.Context, target domain.Target, owner string) error {
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var curMode string
	row := tx.QueryRowContext(ctx,
		`SELECT mode FROM locks WHERE target_canonical = ? AND owner_uuid = ?`,
		target.Canonical, owner)
	if err := row.Scan(&curMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoLockAtTarget
		}
		return err
	}
	if curMode == domain.ModeShared {
		return commitTxFn(tx) // already shared — no-op
	}

	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE locks SET mode = ? WHERE target_canonical = ? AND owner_uuid = ?`,
		domain.ModeShared, target.Canonical, owner); err != nil {
		return err
	}
	if err := appendEventTx(ctx, tx, domain.Event{
		Target:    target,
		Kind:      EventLockDowngraded,
		ActorUUID: owner,
		Reason:    "exclusive→shared",
		CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := commitTxFn(tx); err != nil {
		return err
	}

	// Restore write bit outside the tx — the row is authoritative now.
	if rerr := restoreWrite(target.Canonical); rerr != nil {
		_ = s.appendAuditDetached([]domain.Event{
			modeRestoreFailedEvent(target.Canonical, owner, now, rerr),
		})
		return &ChmodFailureError{Failures: []ChmodFailure{
			{Target: target, Err: rerr, RolledBack: false},
		}}
	}
	return nil
}
```

```go
// internal/store/locks.go — add the const next to the other Event consts:
	EventLockDowngraded = "lock_downgraded"
```

(Use the codebase's existing `appendEventTx` / `commitTxFn` / `appendAuditDetached` helpers — all present in the store package. `errors.Is(err, sql.ErrNoRows)` is the established no-row idiom, see `LockAt`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run TestDowngrade -v` then `go test ./internal/store/...`.
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/locks_downgrade.go internal/store/locks.go internal/store/locks_query.go internal/store/locks_downgrade_test.go
git commit -m "feat(store): DowngradeLock exclusive→shared in place (loto-k5el.2 T5)"
```

---

### Task 6: events CHECK constraint for existing DBs

**Files:**
- Modify: `internal/store/store.go` (add `ensureEventsCheckCurrent`, call from `migrate`)
- Test: `internal/store/migrate_mode_test.go` (extend)

- [ ] **Step 1: Write the failing test**

```go
func TestMigrate_AllowsDowngradeEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events(id,target_canonical,event_kind,actor_uuid,reason,created_at)
		 VALUES ('e-test','/a.go','lock_downgraded','alice','x',0)`)
	if err != nil {
		t.Fatalf("lock_downgraded must be an allowed event_kind: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails / passes**

Run: `go test ./internal/store/ -run TestMigrate_AllowsDowngradeEvent -v`
Expected: On a fresh DB built from the widened `schema.sql` (Task 1 Step 3) this **PASSES** immediately — the CHECK already lists `lock_downgraded`. The failing case is an **existing DB** whose `events` table predates the widening. The guard below handles it; the fresh-DB assertion above plus a legacy-DB fixture (if the harness has one) pin both paths.

‡ **Pragmatic scope call:** the events CHECK is only a problem for DBs created before this change that then receive a `lock_downgraded` event. Because `schemaUserVersion` is not bumped, such a DB is not move-asided. The clean fix is to widen the events CHECK via the same rebuild as locks, guarded by a probe. Implement the guard below (low risk — it's a no-op when the CHECK already contains the kind). If no legacy-DB test fixture exists to exercise the rebuild branch directly, ship it covered by the fresh-DB assertion and note the gap. **Surface to dk as a known migration edge (Open Q4).**

- [ ] **Step 3: Implement the events rebuild guard**

```go
// internal/store/store.go — ensureEventsCheckCurrent (call from migrate after
// ensureLocksModeAndPK). Probe the stored DDL; rebuild only if it lacks the new kind.
func ensureEventsCheckCurrent(ctx context.Context, tx *sql.Tx) error {
	var ddl string
	if err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "lock_downgraded") {
		return nil // already current
	}
	const rebuild = `
CREATE TABLE events_new (
  id TEXT PRIMARY KEY, target_canonical TEXT NOT NULL,
  event_kind TEXT NOT NULL CHECK (event_kind IN (
    'lock_acquired','lock_released','lock_broken','lock_reclaimed_stale',
    'mode_restore_failed','acquire_rollback_started','lock_downgraded')),
  actor_uuid TEXT NOT NULL, subject_uuid TEXT,
  reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
INSERT INTO events_new SELECT id,target_canonical,event_kind,actor_uuid,subject_uuid,reason,created_at FROM events;
DROP TABLE events;
ALTER TABLE events_new RENAME TO events;
CREATE INDEX IF NOT EXISTS idx_events_target     ON events(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_events_kind       ON events(event_kind, created_at);
CREATE INDEX IF NOT EXISTS idx_events_created_id ON events(created_at, id);`
	_, err := tx.ExecContext(ctx, rebuild)
	return err
}
```

Add the call inside `migrate`, after `ensureLocksModeAndPK`. Add `"strings"` to the store.go imports if not already present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestMigrate_AllowsDowngradeEvent -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/migrate_mode_test.go
git commit -m "feat(store): widen events CHECK for lock_downgraded on legacy DBs (loto-k5el.2 T6)"
```

---

### Task 7: CLI — `--shared` flag on `loto lock`

**Files:**
- Modify: `internal/cli/cmd_lock.go`
- Test: `internal/cli/cmd_lock_test.go` (extend)

- [ ] **Step 1: Write the failing test**

```go
// internal/cli/cmd_lock_test.go — adapt to the harness in run_helper_test.go /
// behavioral_cli_test.go (As / Run / RunCode helpers).
func TestCmdLock_SharedFlag_AllowsCoexist(t *testing.T) {
	env := newCLITestEnv(t)
	f := env.TempFile("a.go")
	env.As(alice).Run("lock", f, "-t", "read", "--shared")
	out, code := env.As(bob).RunCode("lock", f, "-t", "read", "--shared")
	if code != 0 {
		t.Fatalf("second shared lock should succeed; code=%d out=%s", code, out)
	}
}

func TestCmdLock_DefaultExclusive_Blocks(t *testing.T) {
	env := newCLITestEnv(t)
	f := env.TempFile("a.go")
	env.As(alice).Run("lock", f, "-t", "write") // default exclusive
	_, code := env.As(bob).RunCode("lock", f, "-t", "write")
	if code != 1 {
		t.Fatalf("default exclusive should block; code=%d", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestCmdLock_SharedFlag|TestCmdLock_DefaultExclusive' -v`
Expected: FAIL — `--shared` unknown flag.

- [ ] **Step 3: Add the flag and thread it through**

```go
// internal/cli/cmd_lock.go — in cmdLock, after the intent flags:
	shared := fs.Bool("shared", false, "acquire a shared (multi-reader) lock; default is exclusive")
	...
	mode := domain.ModeExclusive
	if *shared {
		mode = domain.ModeShared
	}
	return acquireBatch(rt, targets, *intent, *ttl, mode, rt.liveProbe(), stdout, stderr)
```

```go
// acquireBatch signature gains mode; pass it to buildLockRecords:
func acquireBatch(rt *runtime, targets []domain.Target, intent string, ttl time.Duration, mode string, live domain.PidLiveProbe, stdout, stderr io.Writer) int {
	...
	recs := buildLockRecords(targets, rt, intent, now, ttl, mode)
	...
}

// buildLockRecords sets Mode on each record:
func buildLockRecords(targets []domain.Target, rt *runtime, intent string, now time.Time, ttl time.Duration, mode string) []domain.LockRecord {
	...
	recs = append(recs, domain.LockRecord{
		...
		ProcStart:   procStartVal,
		Mode:        mode,
	})
	...
}
```

Update `lockUsageHead` to document `--shared`:

```go
const lockUsageHead = `usage: loto lock <target> [<target>...] -t "why" [--shared]

Acquire a lock on one or more targets. -t (intent) is required.
Default mode is exclusive (sole writer). --shared takes a multi-reader lease.

examples:
  loto lock internal/store/store.go -t "store refactor"
  loto lock README.md -t "reading docs" --shared
`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestCmdLock_SharedFlag|TestCmdLock_DefaultExclusive' -v`
Expected: PASS. Then `go test ./internal/cli/...` — help-golden tests (`help_golden_test.go`, `help_contract_test.go`) will need the usage update; regenerate/adjust the golden text.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_lock.go internal/cli/cmd_lock_test.go internal/cli/help_golden_test.go
git commit -m "feat(cli): loto lock --shared flag (loto-k5el.2 T7)"
```

---

### Task 8: CLI — `loto downgrade` verb + status shows mode + check mode-aware

**Files:**
- Create: `internal/cli/cmd_downgrade.go`, `internal/cli/cmd_downgrade_test.go`
- Modify: `internal/cli/cmd_check.go` (`appendCheckConflictsForTarget` uses `Conflicts`)
- Modify: `internal/cli/cmd_status.go` (show mode; migrate off `LockAt` if used)
- Modify: `internal/render/cli.go` + `cli_test.go` (mode in rows; `EmitLockSuccess` takes records)
- Modify: `internal/cli/help_golden_test.go` (register/list `downgrade`)

- [ ] **Step 1: Write the failing downgrade CLI test**

```go
// internal/cli/cmd_downgrade_test.go
func TestCmdDowngrade_ExclusiveToShared(t *testing.T) {
	env := newCLITestEnv(t)
	f := env.TempFile("a.go")
	env.As(alice).Run("lock", f, "-t", "write") // exclusive
	out, code := env.As(alice).RunCode("downgrade", f)
	if code != 0 {
		t.Fatalf("downgrade should succeed; code=%d out=%s", code, out)
	}
	_, c2 := env.As(bob).RunCode("lock", f, "-t", "read", "--shared")
	if c2 != 0 {
		t.Fatalf("shared lock should succeed after downgrade; code=%d", c2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestCmdDowngrade -v`
Expected: FAIL — `downgrade` unregistered.

- [ ] **Step 3: Implement cmd_downgrade.go**

```go
// internal/cli/cmd_downgrade.go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("downgrade", cmdDowngrade) } //nolint:gochecknoinits // command registry pattern

const downgradeUsageHead = `usage: loto downgrade <target> [<target>...]

Downgrade your exclusive lock(s) to shared, in place — peers may then take
shared locks on the same target without you releasing first. Restores the
file's write bit. A lock that is already shared is a no-op.
`

func cmdDowngrade(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("downgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, downgradeUsageHead) }
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: loto downgrade <target> [<target>...]")
		return 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	targets, invalid := validateLockTargets(fs.Args(), repoTop)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	exit := 0
	for _, t := range targets {
		switch err := rt.Store.DowngradeLock(rt.Ctx, t, rt.Agent.UUID); {
		case err == nil:
			fmt.Fprintf(stdout, "✓ downgraded target=%s mode=shared\n", relPath(t.Canonical))
		case errors.Is(err, store.ErrNoLockAtTarget):
			fmt.Fprintf(stdout, "✗ target=%s reason=no-lock-held\n", relPath(t.Canonical))
			exit = 1
		default:
			var cfe *store.ChmodFailureError
			if errors.As(err, &cfe) {
				fmt.Fprintf(stdout, "⚠ target=%s mode=shared write-bit-restore-failed\n", relPath(t.Canonical))
				exit = 3
				continue
			}
			fmt.Fprintf(stderr, "✗ %v\n", err)
			exit = 3
		}
	}
	return exit
}
```

‡ Output follows design.md: glyph-led, `key=value`, relative paths, deterministic per-target rows. The repo uses `init()`-based `register(...)` (see `cmd_lock.go`), so no `cli.go` edit is needed — but confirm `help_contract_test.go`/`help_golden_test.go` list `downgrade` and update the golden help text.

- [ ] **Step 4: Make check mode-aware**

```go
// internal/cli/cmd_check.go — appendCheckConflictsForTarget
// Replace the manual same-canonical + IsStale skip with the Conflicts predicate.
// The committing agent's intent is to WRITE (it's staging a change), so probe as
// exclusive — a shared peer is then correctly NOT a conflict, an exclusive peer IS.
func appendCheckConflictsForTarget(rows []checkConflict, seen map[string]bool, t domain.Target, all []domain.LockRecord, myUUID string, ec domain.EvalContext) []checkConflict {
	probe := domain.LockRecord{Target: t, OwnerUUID: myUUID, Mode: domain.ModeExclusive}
	for i := range all {
		l := &all[i]
		if !ec.Conflicts(probe, *l) {
			continue
		}
		key := t.Canonical + "|" + l.Target.Canonical + "|" + l.OwnerUUID
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, checkConflict{Path: t.Canonical, Blocker: all[i]})
	}
	return rows
}
```

‡ **Policy note (epic open question):** this keeps `check` a **hard block** (exit 1) on a genuine exclusive-peer conflict, unchanged. It only stops reporting *shared* peers as conflicts — a pure correctness fix. The grant-with-warning softening is left for dk (§ check --staged interaction, Open Q2). Do NOT change the exit code here. The existing `check` tests asserting exit 1 on a real conflict must still pass; add `TestCheck_SharedPeerNotConflict` (alice holds shared, bob `check`s the path → exit 0) and keep an exclusive-peer exit-1 case.

- [ ] **Step 5: status + render show mode**

```go
// internal/cli/cmd_status.go — wherever a lock row is rendered, include the mode.
// If cmd_status uses LockAt (singular), switch to listing all holders at the
// target (filter ListLocks by target) so a multi-holder shared target shows every
// reader. Render each: target, holder, mode, intent, expires_at.

// internal/render/cli.go — EmitLockSuccess currently takes []domain.Target. To
// show mode it must take []domain.LockRecord (or (target,mode) pairs). Update the
// sole call site in cmd_lock.go::acquireBatch (it already has acquired
// []domain.LockRecord before mapping to bare targets — pass the records).
// Success row gains mode:  ✓ locked target=a.go mode=shared
// Keep deterministic field order; add mode after target. Conflict + status rows
// likewise gain mode=<shared|exclusive>.
```

```go
// internal/render/cli_test.go
func TestEmitLockSuccess_ShowsMode(t *testing.T) {
	var buf bytes.Buffer
	render.EmitLockSuccess(&buf, []domain.LockRecord{
		{Target: domain.Target{Canonical: "a.go"}, Mode: domain.ModeShared},
	})
	if !strings.Contains(buf.String(), "mode=shared") {
		t.Fatalf("want mode=shared in: %q", buf.String())
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/cli/... ./internal/render/...`
Expected: PASS (after updating golden help + render assertions + `EmitLockSuccess` call site).

- [ ] **Step 7: Commit**

```bash
git add internal/cli/cmd_downgrade.go internal/cli/cmd_downgrade_test.go internal/cli/cmd_status.go internal/cli/cmd_check.go internal/render/cli.go internal/render/cli_test.go internal/cli/help_golden_test.go
git commit -m "feat(cli): loto downgrade verb, mode in status/render, mode-aware check (loto-k5el.2 T8)"
```

---

### Task 9: end-to-end acceptance + NORTH_STAR

**Files:**
- Create: `internal/cli/acceptance_shared_test.go`
- Modify: `docs/NORTH_STAR.md`

- [ ] **Step 1: e2e scenario test**

```go
func TestAcceptance_SharedExclusiveDowngrade(t *testing.T) {
	env := newCLITestEnv(t)
	f := env.TempFile("a.go")

	// 1. two shared locks coexist
	env.As(alice).Run("lock", f, "-t", "read", "--shared")
	if _, c := env.As(bob).RunCode("lock", f, "-t", "read", "--shared"); c != 0 {
		t.Fatal("shared+shared must coexist")
	}
	// 2. exclusive conflicts with the shared holders
	if _, c := env.As(carol).RunCode("lock", f, "-t", "write"); c != 1 {
		t.Fatal("exclusive must conflict with existing shared holders")
	}
	// 3. release shared holders, then exclusive succeeds
	env.As(alice).Run("unlock", "-t", "done", f)
	env.As(bob).Run("unlock", "-t", "done", f)
	if _, c := env.As(carol).RunCode("lock", f, "-t", "write"); c != 0 {
		t.Fatal("exclusive should acquire once shared holders gone")
	}
	// 4. carol downgrades; dave can then take shared
	if _, c := env.As(carol).RunCode("downgrade", f); c != 0 {
		t.Fatal("downgrade should succeed")
	}
	if _, c := env.As(dave).RunCode("lock", f, "-t", "read", "--shared"); c != 0 {
		t.Fatal("shared should succeed after downgrade")
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/cli/ -run TestAcceptance_SharedExclusiveDowngrade -v`
Expected: PASS.

- [ ] **Step 3: NORTH_STAR §Lock modes**

Append ~20 lines to `docs/NORTH_STAR.md`: the LockMode model, invariants I1–I6, the write-bit rule (stripped iff exclusive), the conflict truth table, the downgrade-only-exclusive→shared scope, and the `check --staged` policy (hard-block on exclusive conflict; grant-with-warning is an open dk decision).

- [ ] **Step 4: Commit**

```bash
git add internal/cli/acceptance_shared_test.go docs/NORTH_STAR.md
git commit -m "test+docs: shared/exclusive e2e + NORTH_STAR (loto-k5el.2 T9)"
```

---

### Task 10: full verification + open the PR

- [ ] **Step 1: Local full suite (note: -race is CI-only on macOS)**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green. (`-race` runs on CI; do not gate locally on it.)

- [ ] **Step 2: lint**

Run: `golangci-lint run` (if phantom-worktree findings appear, verify against real `internal/` and `golangci-lint cache clean` per workflow.md).

- [ ] **Step 3: push + PR (NOT a local merge to main)**

```bash
git push -u origin <feature-branch>
gh pr create --title "feat(loto): shared/exclusive lock modes + downgrade (loto-k5el.2)" \
  --body "Implements loto-k5el.2. Composite PK, mode column, mode-aware conflict predicate, exclusive→shared downgrade, --shared flag + downgrade verb. check --staged stays hard-block on exclusive conflict (grant-with-warning left as dk decision per epic). Coordinated with sibling .1 on the shared lock record — see plan §Reconciliation."
```

- [ ] **Step 4: close the bead on merge**

After CI green + merge: `bd close loto-k5el.2` (or `Closes #N` in the squash commit). If this completes the epic's children, check whether loto-k5el can close too.

---

## Non-goals (this bead)

- **Upgrade shared→exclusive.** Out of scope. Requires re-stripping the write bit AND a fresh conflict check against peer shared holders (which must be gone first). File a follow-up if needed.
- **check --staged grant-with-warning.** Deliberately left as a dk decision (epic open question). This plan keeps the hard block on exclusive conflicts.
- **Auto-downgrade on conflict ("negotiation").** No automatic downgrade when a peer requests; downgrade is an explicit `loto downgrade` call. The "negotiation" framing is realized by *making downgrade cheap and in-place*, not by automating it.
- **TTL / liveness fields.** Owned by sibling loto-k5el.1.
- **Shared-lock count limits.** No cap on concurrent shared holders.
- **Per-mode TTL policy.** Modes don't change TTL behavior.

---

## Open Questions (for dk)

1. **[.1 schema reconciliation — highest priority]** Sibling loto-k5el.1 (TTL/liveness) has no committed schema yet. This plan **assumes** .1 does not change the lock table's PK and that both siblings add columns via additive ALTER without a `schemaUserVersion` bump (the `proc_start` precedent). **If .1's final design changes the PK or restructures `reclaimStaleAndCollectBlockers`, Tasks 1 and 3 need a rebase.** Recommend .1 lands first; .2 rebases its PK change on top. Confirm landing order.

2. **[check --staged policy — the epic's open question]** This plan keeps `check --staged` a **hard block (exit 1)** on a genuine exclusive-peer conflict, only fixing the correctness bug where a *shared* peer was wrongly reported as a conflict. The epic asks whether the pre-commit gate should instead **grant-with-warning** (tell the committing agent "exclusive holder present, wait/ask-to-downgrade" but don't block). That's a one-line change but relaxes a safety gate — **left for you to decide.** Which way?

3. **[PK migration weight]** Changing the locks PK requires a SQLite table rebuild (not an in-place ALTER), heavier than the `proc_start` precedent. The plan does this inside the migrate tx without bumping `user_version`. Confirm you're comfortable with a table-rebuild migration on existing `loto.db` files (atomic and crash-safe, but a bigger migration than loto has done before).

4. **[events CHECK on legacy DBs]** Adding the `lock_downgraded` event kind requires widening the events-table CHECK constraint, which also needs a rebuild for pre-existing DBs (Task 6). The guard is a no-op when the CHECK already contains the kind, but the rebuild branch may lack a direct test if no legacy-DB fixture exists. Acceptable to ship covered by the fresh-DB assertion, or build the fixture now?

5. **[downgrade granularity]** `loto downgrade <target>` downgrades the caller's exclusive lock to shared. There's no `loto downgrade --all`. Is per-target sufficient, or do you want an all-my-locks downgrade?

---

## Self-review

**Spec coverage (bead .2 success criteria + epic asks):**
- ✓ (a) lock-record mode field extending .1's record — Task 1 (schema), Task 2 (domain), §Reconciliation
- ✓ (b) new conflict predicate, shared+shared OK / exclusive-vs-anything conflict — Task 2 `Conflicts` + truth table, Task 3 wiring
- ✓ (c) downgrade exclusive→shared without unlock/relock — Task 5 `DowngradeLock`, Task 8 verb
- ✓ (d) check --staged interaction surfaced not silently decided — §check --staged + Task 8 Step 4 + Open Q2
- ✓ (e) CLI: `--shared` flag on `loto lock` — Task 7
- ✓ (f) test plan mapping each Success Criterion to a test — SC1 shared+shared = `TestAcquire_SharedSharedCoexist` / `TestCmdLock_SharedFlag_AllowsCoexist`; SC2 exclusive conflicts = `TestAcquire_ExclusiveBlocksShared` / `TestCmdLock_DefaultExclusive_Blocks`; SC3 downgrade = `TestDowngrade_ExclusiveToShared_RestoresWriteBit` / `TestCmdDowngrade_ExclusiveToShared`; e2e = `TestAcceptance_SharedExclusiveDowngrade`
- ✓ (g) store-touch-via-PR rule — §PROCESS RULE + Task 10 (push + PR, no local main merge)

**Placeholder scan:** `mustLockRecord`/`deadProbe`/`tmpFile`/`newCLITestEnv`/`RunCode` flagged as "mirror existing harness" with the file to crib from named (`locks_test.go`, `run_helper_test.go`, `behavioral_cli_test.go`). No bare TODO/TBD. `LockAt`-caller audit (Task 5 Step 3) names the grep and likely callers. `sql.ErrNoRows` / `appendEventTx` / `commitTxFn` confirmed as existing store helpers.

**Type consistency:** Mode is a `string` with consts `ModeShared`/`ModeExclusive` (domain). `EffectiveMode()` used consistently in store insert/strip/release and domain Conflicts. `Conflicts(a, l)` arg order (incoming, existing) consistent between domain def (Task 2) and both call sites (Task 3 acquire, Task 8 check). `DowngradeLock(ctx, target, owner)` signature consistent between store (Task 5) and CLI (Task 8). `LockForOwnerAt(ctx, target, owner)` consistent (Task 5). `ReleaseResult.Mode` added Task 4, consumed in `restoreAndAuditReleases`. `EmitLockSuccess` signature change (`[]domain.Target` → `[]domain.LockRecord`) flagged at its sole call site (Task 8 Step 5).

**Known risks:**
- The PK rebuild and events-CHECK rebuild are the riskiest parts; both are atomic-in-tx and guarded by shape probes, but new territory for loto's migration path. Open Q3/Q4.
- Merge collision with sibling .1 on `schema.sql`/`store.go`/`records.go`/`staleness.go`/`locks_acquire.go`. Mitigated by §Reconciliation landing-order recommendation; still requires a human-merged rebase if .1 restructures shared functions.
