# plan.md — loto-ux3.1

bead: `loto acquire / release <path>: process-independent hold primitive`
shape: architectural · profile: craft · author: dispatcher (same actor as implementer)
status: **v2 (post P-review + P-arch-fit triage)**

revision history:
- v0 → v1: P-self pass; reservation-scan decision documented; status/doctor probe added; risk text corrected.
- v1 → v2: P-review (approve-with-revisions) + P-arch-fit (drift-flagged) applied. NS amendment promoted to first-class scope. Doctor + status fixes scoped in-bead. Mechanical naming fixes. Reservation-scan decision reversed (now included). JSON envelope versioned.

## Direction

**This bead introduces the first non-flock *blocking* coordination tier in loto.** Reservations today are advisory (they warn, they don't block). After this bead, an agent's `loto try` returns `ErrHeld` because of a tag's `ExpiresAt`, with no flock held by the holder. That is a categorical change to the model.

The north-star sentence *"Tags are descriptive, flock is authoritative. Every protocol decision flows from this"* and invariant #1 *"flock is truth … never read a tag and trust it for safety"* must be amended **first**, in this same bead, to carve out a record-tier with explicit reasoning. The amendment, not just an addendum, is part of acceptance — without it, the docs and the code disagree on disk and future Claudes will read the contradiction as license to trust tags for other safety decisions.

The carve-out: a tag with non-zero `ExpiresAt` is authoritative for the duration of its TTL because no foreground process is available to hold flock. TTL is the staleness guard (analogous to reservations' role today). flock remains authoritative for foreground holds. Three tiers become four:

| Tier | Truth source | Lifecycle | Other agents |
|------|--------------|-----------|--------------|
| Reservation | tag presence + glob | TTL (lazy) | warns only |
| **Acquire (new)** | **tag with ExpiresAt** | **TTL (lazy), survives process exit** | **blocks** |
| File lock (`try`) | flock | process lifetime | blocks |
| Global lock | flock | process lifetime | blocks |

## Plan

### NS amendment (do first)

A1. Edit `docs/NORTH_STAR.md` to:
   - Update the three-tiers table to four tiers, naming acquire as the first non-flock blocking tier with TTL as the staleness guard.
   - Append to invariant #1 the carve-out: *"Exception: tags carrying a non-zero, unexpired `ExpiresAt` are authoritative for that TTL window. The flock principle still governs foreground holds; TTL governs record-tier holds."*
   - Add one paragraph under §"the model" explaining why record-tier is acceptable: process-independent coordination across two events (e.g., PreToolUse → PostToolUse) cannot be served by flock, which is process-bound. TTL prevents indefinite orphans.

This lands in the same commit series as the code. ✗ amendment-after-merge — that's the contradiction-on-disk failure mode.

### Library layer (`loto` package)

L1. Add `LOTO.AcquirePath(agentID, intent, target string, ttl time.Duration, opts ...TagOptions) (*Tag, []*Reservation, error)`.
   - Take global shared flock.
   - Take file flock (briefly, for atomic write).
   - Read existing tag. If present and `tag.AgentID != agentID` and `!tag.SoftStale()`: release, return `ErrHeld{Tag: tag, Kind: kindFile, Target: target}`.
   - If existing tag is mine OR stale: lazy-reap and proceed.
   - Write new tag with `ExpiresAt = now + ttl`, `PID = os.Getpid()` (descriptive only — won't be consulted for expiry).
   - Compute `ConflictingReservations(target)` — return as second slot (advisory; cheap; the hook adapter needs it).
   - Release flock. Return tag + conflicts.
   - Naming: `AcquirePath` to disambiguate from existing `LOTO.Acquire` (foreground blocking poll). Future cleanup may rename, out of scope.

L2. Add `LOTO.ReleasePath(agentID, target string) error`.
   - Read tag. If absent → return nil (idempotent silent success per bead).
   - If `tag.AgentID != agentID` → return new `ErrNotMine{Tag: tag, Target: target}`.
   - Take file flock briefly, remove tag, release flock. Return nil.

L3. Add `ErrNotMine` type alongside `ErrHeld` / `ErrSystem` in loto.go. Implements `error` and `MarshalJSON` mirroring ErrHeld's shape.

L4. Modify `TryFileLock` (loto.go:158) to consult ExpiresAt:
   - After taking file flock, **before** `lazyReapTag`: read tag. If present, has non-zero ExpiresAt, not stale (uses existing `tag.SoftStale()` from loto.go:118), and `tag.AgentID != agentID` → release flock, return `ErrHeld{Tag: tag, Kind: kindFile, Target: target}`.
   - Same-agent re-Try with valid acquire'd tag: succeeds (own tags don't block self).

L5. Modify `lazyReapTag` (loto.go:338) to honor TTL:
   - New guard: only reap if `pidAlive(tag.PID) == false AND (tag.ExpiresAt.IsZero() OR tag.SoftStale())`.
   - **No bool parameter.** Logic flows from the tag's own state.
   - At the `TryGlobalLock` call site, add a one-line comment: `// global tier is process-lifetime only; TTL respect here is incidental, not a contract`. Documents the door without committing to it.

### Doctor (in-scope, not deferred)

D1. Modify `examineTagPair` in `doctor.go` (around line 273–288) to recognize the record-tier:
   - If flock is acquirable (free), tag is present, `tag.ExpiresAt` is non-zero and not expired → classify as new drift class `LiveAcquired` (or extend `Healthy`, decision at impl per existing class taxonomy). Do **not** classify as `DriftStaleTag`. Do **not** reap under `--repair`.
   - If flock is acquirable, tag is present, `tag.ExpiresAt` is non-zero and **expired** → classify as `DriftStaleTag` as today. Reap under `--repair`. (TTL expiry semantics consistent.)
   - If flock is acquirable, tag is present, `tag.ExpiresAt` is zero → existing behavior (DriftStaleTag).

D2. Add a parallel test `TestDoctorAcquiredHold` in `doctor_test.go`, structurally similar to `TestDoctorSoftStaleHeld` (around line 236), confirming that a valid acquire'd hold is not destroyed by `doctor` or `doctor --repair`.

### Status / display surfaces (in-scope, not probed)

S1. Audit every code path that reads tags and could surface a "free" answer for an acquire'd hold:
   - `cmd/loto/dashboard.go` (or wherever `loto status` is implemented — verify at impl)
   - any place that filters by `pidAlive(tag.PID)` to decide held-vs-free
   - `ReadTag` callers in render layer

   Where the filter exists, change to: **held if** `pidAlive(tag.PID) OR (tag.ExpiresAt non-zero AND !tag.SoftStale())`.

S2. Add a status-level test confirming an acquire'd path renders as held with the same field shape as a try'd hold (the bead's "indistinguishability" requirement).

### CLI layer (`cmd/loto`)

C1. New `acquireCmd()` in `cmd/loto/acquire.go`.
   - `loto acquire <path> [--ttl 5m] [--wait 0s] [--intent "msg"]`
   - Default ttl: 5m. Default wait: 0 (immediate fail on conflict).
   - `--wait > 0`: poll AcquirePath with exponential backoff (minimal local loop, do not refactor `pollAcquire`).
   - Exit codes: 0 success · 1 conflict · 2 usage · 3 wait timeout.
   - Emit JSON via existing `EmitJSON` / LLM via new `EmitLLMAcquired`.

C2. Extend `releaseCmd()` in `cmd/loto/main.go` (~line 333). Restructuring required:
   - Change RunE: branch on `len(args)` and `--all-mine` flag. If both set → exit 2 (usage). If `--all-mine` only → existing behavior. If positional path only → call `ReleasePath`.
   - Update `Use` to `release [path]` and `Long` to describe both forms.
   - Map `ErrNotMine` → exit 1 with structured holder identity. Map IO → exit 3.
   - Emit via new `EmitLLMReleasedPath` for LLM mode (path form).

C3. Register `acquireCmd()` in main.go AddCommand block.

### Render layer (`internal/render`)

R1. Add `EmitLLMAcquired(w io.Writer, AcquireEntry) error` in `internal/render/llm.go`. AcquireEntry mirrors `ReservationEntry` shape; includes target, agent (resolved handle), intent, expires_at, conflicts (slice of conflicting reservation patterns).

R2. Add `EmitLLMReleasedPath(w io.Writer, target string) error`. **Distinct name** — `EmitLLMReleased(w, agent, n, errs)` already exists at llm.go:299; Go has no overloading.

R3. JSON envelope: include `"loto:json:v1"` discriminator on acquire/release-path JSON output (matches the LLM envelope convention). One-time stake; `additions only` for future evolution.

### Tests (red first)

T1. Library unit tests in new `acquire_test.go`:
   - acquire-release roundtrip same agent
   - cross-agent acquire conflict surfaces holder via ErrHeld
   - TTL expiry: acquire with 1s TTL, sleep 1.1s, second-agent try succeeds
   - cross-agent release returns ErrNotMine
   - same-agent re-acquire extends ExpiresAt, returns success (same process)
   - **cross-process-same-agent re-acquire** within TTL window: simulate by setting `tag.PID` to a defunct PID then re-acquire under same agentID → should succeed and refresh ExpiresAt
   - acquire-vs-foreground-flock: while another agent holds via TryFileLock, AcquirePath returns ErrHeld with foreground holder identity (flock contention path)
   - release of un-held path returns nil silently
   - AcquirePath returns ConflictingReservations when a reservation matches target

T2. Doctor test (D2 above): `TestDoctorAcquiredHold`.

T3. Status tests (S2 above): acquire'd path renders as held in status output.

T4. CLI integration tests in `cmd/loto/integration_test.go`:
   - Two agents via env, exec the binary, assert exit codes and JSON shape.
   - --wait timeout = exit 3.
   - JSON output carries `"loto:json:v1"` envelope.

T5. Backward-compat: existing `release --all-mine` and `try --hold` tests must remain green untouched.

## Steps (build order)

1. **NS amendment first** — edit `docs/NORTH_STAR.md` per A1. Commit on its own so the conceptual carve-out lands before the code that depends on it. (Reviewers can object to direction without reading code.)
2. **Test red:** write the cross-agent acquire-blocks-try case (T1.cross-agent + the new TryFileLock behavior). Run, fail.
3. Add `ErrNotMine`. Add `AcquirePath` minimal (writes tag, returns no conflicts yet, no ExpiresAt check in Try). Test still red.
4. Modify `TryFileLock` to check ExpiresAt (L4). Run T1.cross-agent → green.
5. Modify `lazyReapTag` per L5. Verify all call sites; existing tests still green.
6. Add `ReleasePath` (L2). Implement T1 release tests. Green.
7. Add doctor recognition (D1). Implement D2. Green.
8. Audit + fix status surfaces (S1). Implement S2. Green.
9. Add `ConflictingReservations` to AcquirePath (L1 second return). Implement T1 reservation test. Green.
10. CLI: `acquireCmd` (C1) + render (R1, R3). Integration test for happy path.
11. CLI: extend `releaseCmd` (C2) + render (R2). Cross-agent release rejection test.
12. CLI: `--wait` polling. T4 timeout test.
13. /magloop → green.
14. Final sweep: confirm "Files NOT touched" still holds for the residual list.

## Acceptance (from bead, mapped to verification)

- `acquire /tmp/x` → 0 + JSON · T1 + T4.
- Other-agent `try` after acquire → 1 + holder JSON · T1.cross-agent + T4.
- Same-agent `release <path>` clears, then `try` succeeds · T1.roundtrip.
- TTL expiry: 1s → free without manual release · T1.ttl-expiry.
- Cross-agent release rejected, no silent steal · T1.cross-agent-release.
- `release --all-mine` unchanged · T5.
- Idempotent re-acquire extends TTL · T1.re-acquire (same and cross-process).
- Idempotent release of non-held path returns 0 · T1.release-nothing.
- **(excellent)** indistinguishable from --hold via status · S2.
- **(excellent)** indistinguishable from --hold via doctor (new) · D2.
- **(excellent)** TTL stored on lock record, lazy check · L4 + L5 + D1.

## Files

| File | Change |
|------|--------|
| `docs/NORTH_STAR.md` | NS amendment per A1 (lands first). |
| `loto.go` | Add `ErrNotMine`. Modify `TryFileLock` (ExpiresAt check). Modify `lazyReapTag` (TTL-aware, no bool param). Add comment at TryGlobalLock call site. |
| `acquire.go` | Add `AcquirePath`. (Existing `Acquire` blocking-poll method retained.) |
| `release.go` | Add `ReleasePath`. |
| `acquire_test.go` (new) | T1 — library-layer tests including cross-process-same-agent. |
| `doctor.go` | D1 — `examineTagPair` recognizes record-tier. |
| `doctor_test.go` | D2 — `TestDoctorAcquiredHold`. |
| `cmd/loto/acquire.go` (new) | `acquireCmd()` + flags + emit. |
| `cmd/loto/main.go` | C2 + C3 — extend `releaseCmd`, register `acquireCmd`. |
| `cmd/loto/dashboard.go` (or where `loto status` lives — verify at impl) | S1 — TTL-aware held check. |
| `cmd/loto/integration_test.go` | T4 — multi-agent CLI coverage. |
| `internal/render/llm.go` | R1 + R2 + R3 — `EmitLLMAcquired`, `EmitLLMReleasedPath`, v1 envelope. |
| `internal/render/llm_test.go` | Snapshot tests for new emitters + envelope. |

**Files NOT touched (revised explicit list):** `flock_*.go`, `mailbox.go`, `reservation.go`, `pid_*.go`. If implementation drifts here, halt and surface — likely scope expansion.

## Risks / Open questions

1. **Status surface location.** S1 says "verify at impl" — whichever file actually renders held-vs-free for `loto status`. Plan does not pin the file because dashboard.go and llm.go both touch the data; verify before edit.
2. **Race: TTL expires mid-Try.** Agent A acquired with TTL=1s. Agent B's Try reads tag at T=0.99s (not stale) → ErrHeld. Agent B retries at T=1.01s (stale) → succeeds. Acceptable; documented behavior.
3. **JSON envelope adoption.** R3 stakes `loto:json:v1`. If existing JSON outputs (try, status, etc.) don't carry an envelope today, adding one to acquire/release-path means a one-off inconsistency. Choose at impl: either add envelope only to new commands (justify: new contract starts versioned) or add to all (out of scope for this bead — file follow-up).
4. **`--wait` is genuinely new.** No existing `try --wait`. exit 3 sets the precedent for loto-ux3.6's `try --on-timeout`.
5. **Cross-process-same-agent test mechanism.** Cannot fork in unit tests cleanly. Workaround: write a tag with `tag.PID = 1` (init, alive but not loto), then re-acquire — exercises "non-self PID, ExpiresAt fresh, same agentID" → should succeed. Verify pidAlive(1) returns true on test platforms; if not, pick a different sentinel.

## North-star recenter (verbatim per skill)

> "Five Claude Code sessions, same repo, different subtrees, each spawning subagents. All editing files. Today they clobber each other or panic on unexpected diffs. loto exists so any Claude can answer one question fast: 'Is it safe for me to edit this path right now, and if not, who's on it?'"

This bead extends the answer to cover cross-event coordination (PreToolUse → PostToolUse) for the hook-adapter use case. Without the in-bead doctor + status fixes (D1, D2, S1, S2), the answer would diverge by command — `try` says held, `status` says free — directly damaging the one-question test. With them, the bead makes loto's answer correct in a class of cases where today it cannot answer at all.
