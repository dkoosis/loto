# Go Codebase True-Bug Audit (2026-04-29)

> Note: `snipe` was not available in this container (`snipe: command not found`), so reachability traces below use static call paths from the repository source.

## PASS 1 — True Bugs Audit (Correctness & Reliability)

### System Map
- **Entry points**: Cobra CLI commands under `cmd/loto` (`try`, `status`, `reap`, `break`, `whoami`, `release`, `inbox`, `msg`, `reserve`, `doctor`).
- **Concurrency model**: Mostly synchronous CLI calls; lock coordination relies on kernel `flock`; polling loops in `Acquire`/`AcquireGlobal`.
- **Persistence**: Filesystem state under base dir (`files/*.lock`, `files/*.tag`, `files/*.msgs`, `reservations/*.tag`, global lock/tag).
- **External boundaries**: OS syscalls (`flock`, process checks, file IO), `git` command for branch metadata, env/session file probing.
- **Error conventions**: domain errors (`ErrHeld`), operational wrappers (`ErrSystem`), command-level typed exits.

### Findings (ranked)

1. **Mailbox compaction can drop concurrent writes (lost messages)**  
   **Severity:** High  
   **Evidence:** `SendMsg -> appendMsg -> compactFile -> rewriteMsgs` rewrites full mailbox file without lock/synchronization; appends use independent file handles with `O_APPEND`.  
   **Mechanism:** One process rewrites `*.msgs` via temp file+rename while another process appends to old inode; append may be lost after rename or vanish from active path.  
   **Failure scenario:** Two agents message same target near `mailboxCompactAt`; one triggers compaction while another appends. The second message may not appear in final mailbox.  
   **Minimal fix + tests:** Add per-mailbox advisory lock (`*.msgs.lock`) around append+compact; integration test with parallel goroutines/processes asserting no missing sequence IDs.  
   **Confidence:** High.

2. **Corrupt mailbox lines are silently discarded (data loss masking corruption)**  
   **Severity:** Medium  
   **Evidence:** `readMsgs` ignores JSON unmarshal errors per line (`if json.Unmarshal(...) == nil { append }`), no error propagation.  
   **Mechanism:** Truncated/partial writes or manual edits are dropped invisibly; operators cannot detect corruption.  
   **Failure scenario:** Disk-full or interrupted write produces partial JSON line; subsequent reads omit message with no alert.  
   **Minimal fix + tests:** Track parse failures and return structured warning/error (or quarantine bad lines); add tests for malformed lines.  
   **Confidence:** High.

3. **Lock-acquire path misclassifies system errors as contention**  
   **Severity:** Medium  
   **Evidence:** `TryFileLock`/`TryGlobalLock` treat any `flock*` error as `ErrHeld` and read holder tag.  
   **Mechanism:** Non-contention failures (EBADF/EIO/ENOLCK) become “held by X” instead of system error, causing wrong operator action and retry loops.  
   **Failure scenario:** Filesystem/sandbox issue returns syscall error; CLI reports conflict, hiding underlying outage.  
   **Minimal fix + tests:** Distinguish `EWOULDBLOCK`/`EAGAIN` from other flock errors; return `ErrSystem` for non-contention codes. Add syscall-mocking tests.  
   **Confidence:** Medium-High.

4. **Acquire loops allocate timer each iteration (pressure under long contention)**  
   **Severity:** Low (Plausible reliability)  
   **Evidence:** `Acquire`/`AcquireGlobal` use `time.After(interval)` in loop.  
   **Mechanism:** Repeated allocations increase GC churn for long waits and many concurrent waiters.  
   **Failure scenario:** Large CI farm all waiting on same lock; elevated alloc/GC overhead degrades latency.  
   **Minimal fix + tests:** Replace with reusable `time.Timer` and `Reset`; benchmark contention path.  
   **Confidence:** Medium (performance/reliability, not correctness).

## PASS 2 — Concurrency & Lifecycle Audit

### Concurrency Roots Inventory
- Polling waiters in `Acquire` and `AcquireGlobal`.
- Cross-process mailbox writers (`SendMsg`) and compactors (`CompactMsgs` / opportunistic `compactFile`).
- Doctor and release workflows interacting with lock/tag files.

### Findings (ranked)

1. **Mailbox append/compact race causes non-deterministic message loss**  
   **Severity:** High  
   **Evidence + lifecycle trace:** Start: CLI `msg` command -> `SendMsg` -> `appendMsg`; in same path threshold triggers `compactFile` -> `rewriteMsgs` (temp+rename). Parallel process may append during rewrite.  
   **Mechanism:** Missing lifecycle symmetry/ownership for mailbox resource; no mutual exclusion.  
   **Timeline scenario:** T0 P1 append, T1 P2 append threshold reached, T2 P2 rewrites from stale snapshot, T3 rename hides P1 late append.  
   **Minimal fix:** mailbox lock file + critical section for read/append/compact/rename operations.  
   **Test strategy:** multi-process stress test writing monotonic IDs; assert full set present after compactions.  
   **Confidence:** High.

2. **Doctor repair can race active lock transitions (false stale/dead findings)**  
   **Severity:** Medium (Plausible)  
   **Evidence + lifecycle trace:** `Doctor` reads tag then probes lock state (`flockExclusive`) and PID; no generation/version check between reads and repair actions.  
   **Mechanism:** TOCTOU across read-tag / check-lock / remove-tag can act on stale observations when ownership changes rapidly.  
   **Timeline scenario:** Holder releases/reacquires between checks; doctor removes fresh tag thinking stale.  
   **Minimal fix:** Re-read and compare tag content before destructive repair; include timestamp+pid+agent match guard.  
   **Test strategy:** race harness with lock churn plus doctor repair loops.  
   **Confidence:** Medium.

## PASS 3 — Persistence & Boundary Audit

### Boundary Inventory
- **File writes:** tags (`writeTagAtomic`), mailbox appends/rewrites, reservation files.
- **Locking boundary:** flock wrappers (`flock_unix.go`), lock files.
- **Process boundary:** PID liveness checks for doctor/reap logic.
- **Command boundary:** CLI arguments feeding target paths and durations.

### Findings (ranked)

1. **Non-atomic mailbox append + rewrite workflow can violate durability expectations**  
   **Severity:** High  
   **Evidence + boundary trace:** `SendMsg` appends line, compaction rewrites entire file to temp then rename; no fsync on file or directory.  
   **Mechanism:** Crash/power loss can lose acknowledged messages; rename without sync may disappear after reboot on some filesystems.  
   **Failure scenario:** Message command succeeds, node crashes immediately; mailbox lacks last entries post-restart.  
   **Minimal fix:** fsync mailbox fd after append; fsync temp before rename and fsync parent dir after rename.  
   **Test plan:** fault-injection tests with crash simulation (or chaos monkey around rename).  
   **Confidence:** Medium.

2. **Reservation read path drops invalid/corrupt entries silently**  
   **Severity:** Low-Medium  
   **Evidence + boundary trace:** `ListReservations` loops files; if `readReservation` errors, entry skipped (`continue`) with no surfaced error/telemetry.  
   **Mechanism:** Boundary corruption disappears from operator visibility; can hide policy-impacting reservation state.  
   **Failure scenario:** malformed reservation file from partial write; users assume no reservations and proceed into conflict.  
   **Minimal fix:** accumulate/report corrupted files separately; optionally quarantine invalid entries.  
   **Test plan:** inject corrupt reservation `.tag` and verify warning appears in output/log path.  
   **Confidence:** High.
