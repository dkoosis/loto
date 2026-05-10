# lockout primitive — design

*Closes gh#57. Lays groundwork for hook gate (Tasks 21-22) and future tier-3 foreground holds.*

## problem

v2 ships only tagout. `AcquireLock` writes a SQLite row and prints `✓ locked`; nothing on the filesystem prevents another process — Claude or otherwise — from writing the target. False safety is worse than known-absent safety: agents and humans now trust a green checkmark and skip their own caution.

NORTH_STAR.md frames the LOTO metaphor: padlock physically refuses to flip the breaker; tag describes whose lock it is. v2 ships the tag, not the padlock.

## threat model (decided)

**(B) Cooperating Claudes + naive writers.** Other Claude sessions (via hook) plus any tool that respects file permissions — most editors, naive scripts, `make` rules. Defeated by `chmod +w` or `sudo`. Out of scope: hostile/sophisticated bypass, kernel-level enforcement (chattr +i / fanotify / bind-mounts — all require root or daemons NORTH_STAR forbids).

The padlock raises the cost of accidental clobbering to "you had to mean it."

## primitives and what each is for

| Layer | Mechanism | Defends against | Survives process exit? |
|---|---|---|---|
| Tag (descriptive) | SQL row in `locks` table | nothing — informational | yes (DB-resident) |
| **Padlock (enforcement)** | **chmod 0444 on each target** | naive writers, editors honoring perms, cooperating Claudes | **yes** |
| Hook gate (defense in depth) | `loto hook pre-write` consulted by Claude PreToolUse | other Claudes with hook installed | n/a |
| Internal serialization | flock on sidecar `<state>/files/<sha>.opmu`, held only during a `loto lock`/`unlock` operation | races between concurrent loto invocations on the same target | n/a (held only during op) |

What this drops vs. NORTH_STAR's four-tier model: the tier-3 "I'm editing this *right now*" foreground flock is deferred to a future `loto with <cmd>` wrapper. gh#57 closes by chmod, not tier-3.

## scope contract

`loto lock` operates on a **set of regular files, atomically**. One file or many.

- ✓ files only — directories rejected with helpful error: *"`<path>`: not a regular file. loto locks files; for directories, pass the file list (e.g. `loto lock $(fd . internal/store -e go)`)."*
- ✓ multi-file is first-class. `loto lock a.go b.go c.go` → all-or-nothing.
- ✗ no directory targets, no glob expansion at the loto layer, no recursive snapshot semantics. Shell expansion gives Claude the multi-file ergonomics; loto stays language-agnostic.
- ✗ no auto-create placeholder for non-existent paths.

Rejecting directories prevents reproducing the gh#57 false-safety bug at smaller scale: a dir lock that doesn't actually prevent writes to its existing children is the same lie.

## the lock operation

`loto lock <file>...`:

1. For each target, open `<state>/projects/<slug>/files/<sha>.opmu`. `flock(LOCK_EX)` each in canonical-sorted order to avoid deadlock between concurrent invocations.
2. Begin SQL transaction.
3. Reject any non-existent or non-regular-file target with the error above (after releasing flocks).
4. For each target: `stat()` to read current mode → `original_mode`.
5. Collect blockers (existing logic). If any → release flocks, return `ConflictError` with all blockers in JSON.
6. For each target in canonical order: `chmod(0444)`. If any chmod fails → restore modes on already-chmodded targets, abort tx, release flocks, return error.
7. Insert lock rows including `original_mode`.
8. Commit tx, release sidecar flocks.
9. Print `✓ locked <N> file(s)` + structured JSON.

Atomicity: all-or-nothing. The sidecar flocks serialize concurrent `loto lock`/`unlock` of overlapping paths; the SQL tx + chmod-rollback handles the rest.

## the unlock operation

`loto unlock <file>...` (also `loto release --all-mine`):

1. Sidecar flock per target (canonical order).
2. SQL tx: load each row. If any row missing or `owner_uuid != caller` → abort tx, release flocks, return `ErrNotOwner` naming the offending target. **All-or-nothing**: no partial unlock. (Same atomicity contract as lock.)
3. For each target: read `original_mode` from row, `chmod` back. If file is gone (user deleted while locked): no-op. If chmod fails on any target → log and continue (release should not block on fs errors; doctor will catch leftovers).
4. DELETE rows. Commit. Release flocks.
5. Print `✓ unlocked <N> file(s)`.

`loto break --force`: same as unlock but skips owner check, restores mode, appends a `lock_broken` system message to the displaced agent's mailbox (NORTH_STAR invariant 8: no silent dispossession).

## crash recovery

Three orphan classes, reconciled lazily — no daemon:

| State on disk | DB row | What's wrong | Repair |
|---|---|---|---|
| File 0444 | row exists, PID alive | normal hold | none |
| File 0444 | row exists, PID dead or TTL expired + dead | stale lock | `loto doctor --repair` or next acquirer's lazy GC: restore `original_mode`, delete row, append `lock_reclaimed_stale` tag |
| File 0444 | no row | mode-orphan (DB reset, manual delete) | `loto doctor --repair`: cannot know `original_mode` → best-effort restore to 0644 with explicit warning. Recovery-of-last-resort. |
| File 0644 | row exists | someone `chmod +w`'d under us | `doctor` reports; release uses recorded `original_mode` regardless. Threat model (B) → bypass detection out of scope. |

Lazy GC extension: when `collectBlockers` reclaims a stale row, also `chmod` the file back to `original_mode` before deleting the row. Every `loto lock` quietly cleans up after dead holders.

`doctor --repair` becomes the manual sweep for mode-orphans — the case lazy GC can't catch.

## schema change

```sql
ALTER TABLE locks ADD COLUMN original_mode INTEGER;
```

NULL-tolerant. Pre-lockout rows become NULL on upgrade → release deletes the row without chmod (same as today, no silent breakage). New rows always populate.

Migration mechanism: defer to writing-plans — will check what v2 has (`PRAGMA user_version` versus a migration framework) before specifying.

## file boundaries

```
internal/domain/
  records.go        + OriginalMode *uint32 on LockRecord  (nil = unknown)
internal/store/
  locks.go          AcquireLock/ReleaseLock take []Target;
                    chmod calls; sidecar flock calls
  flock_unix.go     NEW: acquireOpMu / releaseOpMu (sidecar flock helpers)
  flock_other.go    NEW: build-tag !unix stub returning error
  chmod.go          NEW: saveAndLockMode / restoreMode
  schema.sql        + original_mode column
  doctor.go         + mode-orphan scan, + lazy-GC mode restore
internal/cli/
  cmd_lock.go       N positional args, multi-target acquire
  cmd_unlock.go     N args
  cmd_break.go      mode restore on break
  cmd_doctor.go     wire mode-orphan repair
internal/render/
  llm.go            multi-file ✓/✗ output + blocker JSON
```

## acceptance tests (the gh#57 fix)

```
internal/cli/acceptance_test.go
  TestLockPreventsDirectWrite                # chmod blocks os.WriteFile from another goroutine/proc
  TestUnlockRestoresMode                     # 0644 → lock → 0444 → unlock → 0644
  TestLockMultiFileAtomic_ConflictAborts     # one conflict → none acquired, modes unchanged
  TestStaleLockReclaimRestoresMode           # lazy GC chmod-restore
  TestDoctorRepairsModeOrphan                # row gone, file 0444 → repaired (with warning)
  TestBreakForceRestoresMode                 # --force restores on break
  TestConcurrentLockSerializesOnSidecar      # two overlapping `loto lock` → no torn original_mode
  TestRejectDirectoryTarget                  # dir → clear error, nothing chmodded
  TestRejectNonExistentTarget                # missing file → clear error, nothing chmodded
```

## non-goals (this PR)

- Tier-3 foreground flock (`loto with <cmd>`).
- Hook gate (gh#57 v2 plan Tasks 21-22 — unblocks once this lands).
- Glob/dir/recursive locking primitives.
- Detecting `chmod +w` bypass.
- Multi-host coordination.

## invariants preserved

- ✓ no daemon
- ✓ single-host
- ✓ reads remain free (chmod 0444 still readable)
- ✓ JSON-first I/O, stable exit codes
- ✓ identity per session (LOTO_AGENT_ID untouched)
- ✓ no silent dispossession (break still notifies via mailbox)
- ✓ tag is descriptive, padlock is enforcement — clean LOTO mapping
