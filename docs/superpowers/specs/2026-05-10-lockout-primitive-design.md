# lockout primitive — design

*Closes gh#57. Folds gh#46 (release distinguishes missing from not-owner). Lays groundwork for hook gate (Tasks 21-22) and future tier-3 foreground holds.*

## problem

v2 ships only tagout. `AcquireLock` writes a SQLite row and prints `✓ locked`; nothing on the filesystem prevents another process — Claude or otherwise — from writing the target. False safety is worse than known-absent safety: agents and humans now trust a green checkmark and skip their own caution.

## threat model

**Cooperating Claudes + naive writers.** Other Claude sessions (via hook, future) plus any tool that respects file permissions — most editors, naive scripts, `make` rules. Defeated by `chmod +w` or `sudo`. Out of scope: hostile/sophisticated bypass, kernel-level enforcement.

## mechanism

| Layer | Mechanism | Defends against | Persists across process exit? |
|---|---|---|---|
| Tag (descriptive) | SQL row in `locks` table | nothing — informational | yes |
| **Enforcement** | **chmod strip-write on each target** | naive writers, editors honoring perms, cooperating Claudes | **yes** |
| Hook gate (future, defense in depth) | `loto hook pre-write` from Claude PreToolUse | other Claudes with hook installed | n/a |
| Internal serialization | One project-wide flock during the operation: `<state>/projects/<slug>/lock-op.flock` | races between concurrent `loto lock`/`unlock` invocations | n/a (held only during op) |

Tier-3 foreground flock (NORTH_STAR's "I'm editing this *right now*") is deferred to a future `loto with <cmd>` wrapper.

## chmod policy (no stored mode)

```
lock:   chmod(path, mode &^ 0222)   # strip write bits
unlock: chmod(path, mode | 0200)    # restore owner-write
```

Round-trips `0644`, `0664`, `0600`, `0640` for owner-write recovery. Group/other write bits are not preserved across a lock cycle — rare on a single-user dev box, recoverable with `chmod g+w` after. The simplification (no `original_mode` column, no migration, no stored state) is worth the lossy round-trip.

**TOCTOU:** external mode changes during the `stat`→`chmod` window can be clobbered. Window is two adjacent syscalls; small enough we don't care for a single-user hand-tool.

**Pathological case:** if the user manually `chmod 0000`s a locked file mid-hold, unlock leaves it at `0200` (write-only, no read). Contrived; acknowledged.

## scope contract

`loto lock` operates on a **set of regular files, atomically**. One file or many.

- ✓ files only — directories rejected with: *"`<path>`: not a regular file. loto locks files; for directories, pass the file list (e.g. `loto lock $(fd . internal/store -e go)`)."*
- ✓ multi-file is first-class. `loto lock a.go b.go c.go` → all-or-nothing acquire.
- ✗ no directory targets, no glob expansion at the loto layer, no recursive snapshot.
- ✗ no auto-create placeholder for non-existent paths.

Rejecting directories prevents reproducing the gh#57 false-safety bug at smaller scale.

## the lock operation

`loto lock <file>... [flags]`

All existing flags (`--intent`, `--ttl`, `--branch`, etc.) apply uniformly to every target in the invocation. No per-target overrides; a caller needing heterogeneous values runs `loto lock` multiple times.

Pseudocode:

```
flock(LOCK_EX, lock-op.flock)                   # blocking; SIGKILL releases
for path in argv:
    validate: exists, regular file              # exit 2 on any rejection,
                                                # zero disk side effects
BEGIN tx
blockers = collectBlockers(targets)
if blockers:
    release op-flock
    emit "✗ blocked count=<N>" + per-blocker rows (see output section)
    exit 1

for i in canonical-sorted(targets):
    stat(path_i)
    chmod(path_i, mode_i &^ 0222)
    on chmod failure:
        for j < i: chmod(path_j, mode_j | 0200)             # rollback
        if any rollback-chmod fails:
            for each unrestored path p:
                insert system tag (event=mode_restore_failed,
                                   addressee=caller, target=p,
                                   intent=<errno>)
            abort tx; release op-flock
            emit per-target failure rows
            exit 3
        else:
            abort tx; release op-flock
            emit per-target failure rows (rolled-back)
            exit 3
        return

INSERT rows  (no original_mode column)
COMMIT
release op-flock

emit:
  ✓ locked count=<N>
  ✓ target=<p1>
  ✓ target=<p2>
  ...
```

Note: validation runs **before** `BEGIN tx`. Don't take a resource we might not need.

## the unlock operation

`loto unlock <file>...` (also `loto release --all-mine`):

**Best-effort, per-target.** Atomicity belongs to acquire, not release.

1. `flock(LOCK_EX)` the project op-flock.
2. For each target, in canonical order:
   - `SELECT owner_uuid FROM locks WHERE target_canonical = ?`
   - No row → emit `ℹ no-lock target=<p>`, continue.
   - Owner ≠ caller → emit `✗ not-owner target=<p> holder=<handle>`, continue. (Folds gh#46: missing and wrong-owner are now distinct outputs.)
   - Owner = caller → `chmod(mode | 0200)` (no-op if file is gone), `DELETE` row, emit `✓ unlocked target=<p>`.
3. Release op-flock.
4. First body line: `✓ unlocked count=<N>` (where N = successful releases). Exit 0 if zero `not-owner` rows seen, 1 otherwise.

`loto break --force`: same chmod-restore + tag append (`lock_broken`), skips owner check, notifies displaced agent via mailbox (NORTH_STAR invariant 8).

## output shapes (failure paths)

Per `design.md`: triage count first, deterministic per-row, key=value, no pluralized prose.

**Conflict** (acquire blocked by other holders, exit 1):
```
✗ blocked count=2
⚠ target=a.go blocker=GreenCastle intent="store refactor" expires_at=2026-05-10T18:00:00Z
⚠ target=c.go blocker=RedRiver    intent="cli cleanup"    expires_at=2026-05-10T17:42:00Z
```

**Chmod failure mid-operation** (exit 3):
```
✗ chmod-failed count=1
✗ target=b.go errno=EPERM rolled-back=yes
✓ target=a.go state=restored
```

If rollback itself fails on any path, that path's row shows `rolled-back=no` and a `mode_restore_failed` system tag is inserted (addressee = caller, target = the unrestored path). The tag's `intent` column reuses the existing human-description slot: `"mode_restore_failed: EPERM on <path>"`. No schema change for the tag. `doctor` surfaces these tags.

**Validation failure** (non-regular, missing — exit 2):
```
✗ invalid count=1
✗ target=internal/store/ reason=not-regular-file
```

**Unlock with mixed outcomes** (exit 1 if any not-owner):
```
✓ unlocked count=4
✓ target=a.go
✓ target=b.go
ℹ target=c.go state=no-lock
✗ target=d.go state=not-owner holder=BlueOak
✓ target=e.go
```

**JSON:** `--json` flag (or non-TTY stdout) emits the same content as a single JSON object. Schema is the structural mirror of the text rows — confirm exact shape during implementation in `internal/render/llm.go`.

## crash recovery

Two orphan classes, both lazily reconciled — no daemon:

| State on disk | DB row | Repair |
|---|---|---|
| stripped-write mode | row exists, PID alive | normal hold |
| stripped-write mode | row exists, PID dead or TTL expired + dead | `doctor --repair` or next acquirer's lazy GC: `chmod(mode \| 0200)`, delete row, append `lock_reclaimed_stale` tag |
| stripped-write mode | no row | `doctor` flags `⚠ orphan-mode target=<p>`. Repair requires explicit `loto doctor --repair --restore-orphan-mode`. Default repair pass does not touch orphan-mode files. |

Operator's explicit intent restores orphan-mode bytes, not loto's heuristic. NORTH_STAR's "no silent dispossession" applies to bytes, not only locks.

Lazy GC extension: when `collectBlockers` reclaims a stale row, also `chmod(mode | 0200)` before deleting the row. Every `loto lock` quietly cleans up after dead holders.

**Side-effect asymmetry:** `reclaimStaleTx` chmods inside the acquire tx. If the surrounding acquire tx later aborts — conflict on a *different* target, chmod failure on a different target, any error path — the stale-reclaim chmod-restore is not rolled back. The reclaimed file stays writable and the stale row may reappear in DB after rollback. Acceptable: stale-and-dead-holder rows are not protecting anyone, so early restoration is benign. Documented so an implementer doesn't mistake it for a bug.

## migration

v2 has no real users. Wipe on schema bump.

- Bump `PRAGMA user_version`.
- On mismatch in `Open`: `MoveCorruptAside(db_path)` (existing pattern), create fresh schema.
- Three lines of code, zero NULL-tolerance complexity. Pre-lockout rows had no chmod underneath them anyway.

## path hash

State sidecar paths (`lock-op.flock`, and any future per-target sidecars) hash the **canonical relative path** as produced by `domain.Canonicalize` (`internal/domain/target.go:31`). Project-scoped state directory disambiguates across projects; no need for absolute paths.

**Follow-up:** NORTH_STAR's "single canonical base" section currently shows `sha256(abs-path)`. Tracked: `loto-9ky`.

## schema change

None. (Was `ALTER TABLE locks ADD COLUMN original_mode` in the prior draft; deleted with the chmod-policy simplification.)

## file boundaries

```
internal/store/
  locks.go          AcquireLock/ReleaseLock take []Target;
                    chmod calls; project op-flock acquire/release
                    ReleaseLock: per-target, best-effort, gh#46 fix here
  flock.go          NEW: project op-flock helpers (unix only)
  chmod.go          NEW: stripWrite / restoreWrite using stat + bitmask
                    chmodFn is a package-private var (default: os.Chmod) so
                    TestChmodRollback_FailureExits3 can inject EPERM
                    without an OS-specific fixture
  schema.sql        bump user_version; no column changes
  doctor.go         + orphan-mode scan (flag-only by default),
                    + --restore-orphan-mode flag,
                    + lazy-GC chmod-restore in reclaim path
internal/cli/
  cmd_lock.go       N positional args, multi-target atomic acquire
  cmd_unlock.go     N args, best-effort
  cmd_break.go      chmod-restore on break
  cmd_doctor.go     wire --restore-orphan-mode
internal/render/
  llm.go            multi-file output per design.md:
                    triage count first; row-per-target; key=value
```

Windows: out of scope. unix-only (`//go:build unix`). No Windows stub file.

## acceptance tests

```
internal/cli/acceptance_test.go
  TestLockedFile_WriteByThirdParty_ReturnsEACCES    # os.WriteFile against 0444 → EACCES (third party, not lock holder)
  TestLockedFile_StillReadable                      # NORTH_STAR invariant 6
  TestLockedFile_ChmodPlusWAllowsWrite              # documents threat-model bypass
  TestUnlock_RestoresOwnerWrite                     # 0644 → lock → 0444 → unlock → 0644
  TestLock_MultiFileAtomic_ConflictAborts           # one conflict → none acquired, no chmod side effects
  TestUnlock_BestEffort_OneNotOwnerReleasesRest     # 5 targets, 1 not-owner → 4 released, exit 1
  TestUnlock_MissingVsNotOwner_DistinctOutput       # gh#46
  TestStaleLockReclaim_RestoresMode                 # lazy GC chmod-restore in collectBlockers
  TestDoctor_OrphanModeFlaggedNotRepaired           # default --repair leaves orphan-mode alone
  TestDoctor_RestoreOrphanModeFlagRepairs           # explicit flag restores
  TestBreakForce_RestoresMode                       # --force chmods back, notifies via mailbox
  TestConcurrentLock_SerializedByOpFlock            # two overlapping `loto lock` invocations don't interleave
  TestRejectDirectoryTarget                         # dir → clear error, zero side effects on disk
  TestRejectNonExistentTarget                       # missing file → clear error, zero side effects on disk
  TestChmodRollback_FailureExits3                   # contrived: rollback fails → mode_restore_failed tag, exit 3
  TestLockMultiFile_FlagsApplyToAllTargets          # --intent/--ttl recorded identically on every target row
```

## existing tests

`internal/cli/cmd_lock_test.go` asserts the old single-target verb (`fs.NArg() != 1`). Port these tests to the multi-target shape; don't keep them as a separate suite.

## doc-debt tracking

Two NORTH_STAR.md edits become true only after this lands. Both are tracked as separate beads (filed alongside this spec):

- `loto-9ky` — NORTH_STAR `sha256(abs-path)` → `sha256(canonical-rel-path)` in the layout block.
- `loto-qy6` — NORTH_STAR operating-loop example `loto try file <path>` → `loto lock <path>...`.

Each is a one-line edit; queued so docs and code don't desync.

## smell-test acknowledgements

- **Op-flock blocking forever:** if a `loto` invocation hangs, all subsequent ones block. flock releases on process exit, so `SIGKILL` recovers. Acceptable for a hand-tool. If pain emerges, swap blocking for `LOCK_NB` + bounded retry.
- **mode-bit lossiness:** see `## chmod policy`.

## non-goals (this PR)

- Tier-3 foreground flock (`loto with <cmd>`).
- Hook gate (gh#57 v2 plan Tasks 21-22 — unblocks once this lands).
- Glob/dir/recursive locking primitives.
- Detecting `chmod +w` bypass.
- Multi-host coordination.
- Windows support.
- NORTH_STAR edits (tracked: `loto-9ky`, `loto-qy6`; see *doc-debt tracking*).

## invariants preserved

- ✓ no daemon
- ✓ single-host
- ✓ reads remain free (chmod strip-write still readable)
- ✓ JSON-first I/O, stable exit codes (0/1/2/3)
- ✓ identity per session
- ✓ no silent dispossession of locks (break notifies via mailbox)
- ✓ no silent dispossession of bytes (orphan-mode repair requires explicit flag)
- ✓ tag is descriptive, chmod is enforcement — clean LOTO mapping
