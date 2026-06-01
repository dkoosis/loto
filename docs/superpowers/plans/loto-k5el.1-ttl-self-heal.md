# loto-k5el.1: TTL auto-expiry on locks (self-healing stale claims) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Bead:** loto-k5el.1 (parent epic loto-k5el)

**Goal:** Make a crashed/abandoned agent's lock self-heal with no manual `loto doctor`, and surface each lock's liveness verdict + remaining TTL in `loto status`.

**Architecture:** Liveness-primary, TTL-as-backstop — exactly as the bead's design guidance pins it. **Critical finding from grounding in the real code (see "State of the world" below): the schema, the liveness probe, the lazy reaping in `lock`/`check`, the durable-session-PID trap fix, the PID-reuse defense, and the SessionEnd eager-release are ALL already shipped across prior beads (loto-t1tq, loto-j1bo, loto-kwlp, loto-9t0q, loto-vtg6, loto-l3as).** This plan is therefore a **gap-closing + read-time-surfacing** plan, not a greenfield build. The two real gaps are: (1) `loto status` neither reaps nor shows owner-alive / TTL-remaining (the **only unmet Success Criterion**, SC#3); (2) the mid-edit-expiry hazard has a de-facto policy in the code but it is undocumented and has no renewal escape hatch. We close both, add the missing tests that pin each Success Criterion, and explicitly decide the mid-edit policy.

**Tech Stack:** Go, SQLite (`loto.db`, existing schema — **no schema bump needed**), packages `internal/domain` (pure predicates), `internal/store` (persistence), `internal/cli` (commands), `internal/render` (output). House output rules: `.claude/rules/design.md` (glyph-led, key=value rows, RFC3339 UTC, deterministic sort, `file:line:col`/relative paths, stdout audience = Claude).

---

## ‡ Hard constraint: store-touch ships via PR

`.claude/rules/workflow.md`: **any change under `internal/store/*` or `internal/identity/registry.go` ships via PR, never direct-to-main** — linux `go test -race ./...` runs CI-only (self-hosted serial runners `mac-loto`/`trixi-loto`, matrix linux+macos). This plan touches `internal/store/locks_query.go` (Task 4) and `internal/domain/*` + `internal/cli/*`. **The whole change ships as ONE PR on branch `plan/loto-k5el.1`'s implementation follow-on** — do not push any store-touching commit to main. A merge backlog of ~15–20 min on the runners is lag, not breakage.

---

## State of the world (read this before writing any code)

The grounding pass read the actual lock implementation. Here is what already exists, with file references, so the implementer does **not** rebuild it:

### Schema — `internal/store/schema.sql`
`locks` table already carries every field this bead asks for:
```
target_canonical TEXT PRIMARY KEY,
owner_uuid TEXT, session_uuid TEXT, intent TEXT,
created_at INTEGER, expires_at INTEGER,    -- ← TTL backstop field, already here
host TEXT, pid INTEGER,
proc_start INTEGER,                          -- ← owner-liveness key (PID-reuse defense), already here
branch TEXT
-- indexes incl. idx_locks_expires, idx_locks_session
```
**No schema change is required.** `expires_at` = TTL backstop. `pid` + `proc_start` + `host` + `session_uuid` = the owner-liveness handle the design guidance asks for.

### Domain predicate — `internal/domain/staleness.go`, `records.go`
`EvalContext.IsStale(LockRecord)` is the liveness-primary-with-TTL-backstop core, already correct:
```go
func (c EvalContext) IsStale(l LockRecord) bool {
	if !c.Now.Before(l.ExpiresAt) {       // TTL backstop fired
		return true
	}
	// PID<=0 sentinel = no durable liveness → TTL is sole authority, never instant-stale
	if l.PID > 0 && l.Host == c.ThisHost && c.Live != nil && !c.Live(l.Host, l.PID, l.ProcStart) {
		return true                        // owner provably gone → instant self-heal
	}
	return false
}
```
`PidLiveProbe func(host string, pid int, storedStart int64) bool` — remote hosts treated live; local pid checked via `pidLive`; `storedStart` defeats PID reuse.

### The CLI-process-PID trap — ALREADY SOLVED — `internal/cli/stamppid.go`
The bead flags the trap: "the stored PID is the ephemeral `loto lock` CLI process that exits immediately, so PID-liveness on that pid is wrong." This is fixed. `stampPID()` reads **`LOTO_PID`** (exported by the SessionStart hook = the long-lived Claude **session** process), NOT `os.Getpid()`:
- valid positive `LOTO_PID` → `(pid, pidDurable)`: liveness binds to the session, which outlives the one-shot CLI → peer can fast-reclaim when the session dies.
- unset/invalid → `(0, pidUnset|pidInvalid)`: the **PID-0 sentinel**. Stamping the dying CLI pid would make the lock instantly reclaimable (loto-t1tq), so pid stays 0 and liveness degrades to TTL-only (loto-j1bo). `degradedPidWarning()` emits a one-line stderr notice in that case.

So **owning-SESSION liveness, not CLI-process-pid liveness, is what the code already probes.** This plan must NOT regress that — every new test asserting reaping uses the PID-0 sentinel path or a fake probe, never `os.Getpid()`.

### Lazy reaping — ALREADY SHIPPED in `lock` and `check`
- **`lock` (acquire):** `internal/store/locks_acquire.go: reclaimStaleAndCollectBlockers` runs `IsStale` per blocker inside the acquire tx and `reclaimStaleTx`-deletes stale rows before evaluating conflicts. So acquiring a target whose holder is dead/expired silently reclaims it — **no doctor run** (SC#1 mechanism already present).
- **`check`:** `internal/cli/cmd_check.go: appendCheckConflictsForTarget` skips any holder where `ec.IsStale(*l)` (loto-9t0q), using `rt.liveProbe()`. So the pre-commit guard already treats a reclaimable lock as non-blocking.
- **`doctor`:** `internal/store/doctor.go: DoctorRepair` / `DoctorAuditWith` is now the **fallback** sweep, not the primary path — exactly the bead's target end-state.

### Clean-exit release — ALREADY SHIPPED — `internal/cli/sessionend_hook_test.go` (loto-l3as)
`.claude/settings.json` registers a `SessionEnd` hook running `loto unlock --all ... || true`, pinned by `LOTO_AGENT_ID`. A clean session exit reclaims immediately instead of waiting out TTL. (The crash/kill path is owned by pid-liveness + TTL — the complementary mechanism.)

### TTL default — `internal/cli/cmd_lock.go`
`--ttl` flag, default `30 * time.Minute`. `buildLockRecords` sets `ExpiresAt: now.Add(ttl)` and stamps `pid`/`proc_start` only when `src == pidDurable`.

### What is NOT yet built (the actual work)
1. **`loto status` does not reap and does not show liveness verdict or remaining TTL.** `internal/cli/cmd_status.go: printStatusLocks` prints raw `expires_at=<RFC3339>` and `pid=`, with **no** owner-alive verdict and **no** TTL-remaining duration. It never consults `IsStale`/`liveProbe`. This is SC#3 and the bulk of the work.
2. **No renewal / refresh escape hatch** for the mid-edit-expiry hazard, and the existing de-facto policy ("backstop only fires when liveness is indeterminate; a live durable-PID holder is never TTL-reaped because the probe says alive") is **undocumented**. We document it and add an optional `loto lock --renew` re-stamp.
3. **No tests pinning the three Success Criteria end-to-end** through the CLI.

---

## Success Criteria → Task map (self-review anchor)

| Success Criterion (from bead) | Status before | Task that pins it |
|---|---|---|
| SC1: acquire a lock with TTL; after expiry another agent acquires same target, no manual doctor | mechanism shipped (acquire reclaim) | **Task 1** (e2e test only) |
| SC2: a killed agent's lock expires within the TTL window / liveness frees it; a live agent's lock stays fresh | mechanism shipped (IsStale + probe) | **Task 2** (e2e test only) |
| SC3: `loto status` shows remaining TTL per lock | **NOT built** | **Task 3, 4, 5** |
| Mid-edit-expiry policy decided + documented (design guidance) | undocumented | **Task 6** (docs) + **Task 7** (`--renew`) |

---

## File Structure

- `internal/domain/staleness.go` — **add** pure helpers `LockLiveness` enum + `Classify(LockRecord) Liveness` + `RemainingTTL(LockRecord) time.Duration`. Pure, no I/O — domain may depend only on stdlib (per `.go-arch-lint.yml`). (Task 4 — domain, NOT store, so no `-race`-only risk, but bundled into the same PR for cohesion.)
- `internal/store/locks_query.go` — **read only** to confirm `ListLocks` shape; no change unless Task 5 needs a reap-on-read variant (it does not — reaping stays in `lock`/`check`/`doctor`; `status` *classifies* but must not mutate, see Task 6 decision). ‡ store-touch → PR.
- `internal/cli/cmd_status.go` — **modify** `printStatusLocks` + `statusSingleTarget` to render liveness verdict + remaining TTL via the new domain helpers and `rt.liveProbe()`.
- `internal/cli/cmd_lock.go` — **modify** to add `--renew` flag wiring (Task 7).
- `internal/cli/cmd_status_test.go` — **modify**: add status-shows-TTL + status-shows-liveness tests.
- `internal/cli/cmd_lock_test.go` — **modify**: add the SC1/SC2 e2e reclaim tests + `--renew` test.
- `internal/domain/staleness_test.go` — **modify**: add `Classify`/`RemainingTTL` unit tests.
- `README.md` — **modify**: document the mid-edit-expiry policy + status liveness columns.

---

## Formal model (claudish) — what `status` will report

```
Now            = wall clock at status invocation
Probe(l)       = l.Live verdict = liveProbe(l.Host, l.PID, l.ProcStart)   -- only meaningful when l.PID>0 ∧ l.Host=thisHost

Liveness(l) ∈ { ALIVE, DEAD, UNKNOWN }
  DEAD     ≡ ¬Now.Before(l.ExpiresAt)                        -- TTL backstop already fired
           ∨ (l.PID>0 ∧ l.Host=thisHost ∧ ¬Probe(l))         -- owner provably gone
  UNKNOWN  ≡ ¬DEAD ∧ (l.PID≤0 ∨ l.Host≠thisHost)             -- no durable liveness handle → TTL is sole authority
  ALIVE    ≡ ¬DEAD ∧ l.PID>0 ∧ l.Host=thisHost ∧ Probe(l)    -- owner session probed live

  NOTE: DEAD ⟺ domain.IsStale(l) under the same EvalContext. Classify is the
  display-tier refinement of IsStale: it splits ¬stale into ALIVE vs UNKNOWN so
  the cause of the verdict is visible (design guidance pt 4).

RemainingTTL(l) = max(0, l.ExpiresAt − Now)                  -- 0 ⟺ TTL backstop fired
```

Invariants the tests pin:
```
I1: Classify(l)=DEAD  ⟺  IsStale(l)               -- status verdict agrees with the reaper
I2: Classify(l)=ALIVE ⟹ a peer `lock` would block (not reclaim) on l
I3: Classify(l)=DEAD  ⟹ a peer `lock` reclaims l with no doctor run
I4: RemainingTTL(l)=0 ⟺ TTL backstop has fired (Now ≥ ExpiresAt)
I5: status NEVER mutates the lock store (read-only command; reaping is lock/check/doctor's job — Task 6 decision)
```

---

## Tasks

### Task 1: e2e test — SC1 (TTL/dead-owner self-heal on acquire, no doctor)

**Files:**
- Test: `internal/cli/cmd_lock_test.go` (append)

This Success Criterion's *mechanism* already ships (acquire-time reclaim). We only need a CLI-level test proving a second agent acquires a target whose holder is reclaimable, with **no `loto doctor` call between**.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/cmd_lock_test.go`. Use the existing test helpers (`withTempProject`, `pinAgent`, `tcTargetA`, `tcIntentTest`, `Run`). The first lock is placed with **no durable LOTO_PID** (PID-0 sentinel) and a **negative/zero TTL** so it is born already past `expires_at` → `IsStale` true → reclaimable. The second agent then acquires it.

```go
// TestAcquireReclaimsExpiredHolder_NoDoctor pins loto-k5el.1 SC1: after a
// holder's TTL has lapsed, a second agent acquires the same target with NO
// intervening `loto doctor`. Mechanism: AcquireLocks→reclaimStaleAndCollectBlockers.
func TestAcquireReclaimsExpiredHolder_NoDoctor(t *testing.T) {
	withTempProject(t)

	// Agent A locks with an already-expired TTL and the PID-0 sentinel
	// (no LOTO_PID), so liveness degrades to TTL and the lock is born stale.
	t.Setenv("LOTO_PID", "") // force pidUnset → PID-0 sentinel, TTL-only liveness
	pinAgentAs(t, "alice")   // see helper note below
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest, "--ttl", "-1s"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice initial lock failed")
	}

	// Agent B acquires the same target. No doctor run between.
	pinAgentAs(t, "bob")
	var out, errb bytes.Buffer
	code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest}, &out, &errb)
	if code != 0 {
		t.Fatalf("bob acquire over expired holder should succeed, got exit %d: out=%q err=%q",
			code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "✓") {
		t.Errorf("expected success glyph in acquire output: %q", out.String())
	}
}
```

> **Helper note:** if `pinAgentAs(t, name)` does not exist, the existing `pinAgent(t)` pins a single fixed agent. Check `internal/cli/run_helper_test.go` / `testconsts_test.go` for the two-agent pattern (the store tests use `tcAlice`/`tcBob`). If only single-agent pinning exists at the CLI layer, add a minimal `pinAgentAs` helper in `run_helper_test.go` that sets `LOTO_AGENT_ID` to a per-name value before `Run`. Do NOT invent store internals — reuse `pinAgent`'s mechanism, parameterized by name.

- [ ] **Step 2: Run test to verify it fails (or passes) — and READ which**

Run: `go test ./internal/cli/ -run TestAcquireReclaimsExpiredHolder_NoDoctor -v`
Expected: Likely **PASS immediately** (mechanism exists). If it passes, that is the correct outcome — the test now *pins* SC1 against regression. If it FAILS, the failure tells you the acquire-reclaim path regressed or the helper is wrong; fix the test setup (env/helper), not the production code, unless the failure is a genuine reclaim bug.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_lock_test.go internal/cli/run_helper_test.go
git commit -m "test(cli): pin loto-k5el.1 SC1 — acquire reclaims expired holder, no doctor"
```

---

### Task 2: e2e test — SC2 (dead session reclaimed; live session NOT reclaimed)

**Files:**
- Test: `internal/cli/cmd_lock_test.go` (append)

SC2 has two halves: a killed agent's lock frees within the TTL window (liveness), and a live agent's lock stays fresh. The liveness probe is injected, so we don't actually kill a process — we drive `IsStale` through a fake probe at the **store** layer where the probe is a parameter, OR through the CLI with a controllable PID. The cleanest CLI-level proof: lock with a **durable PID that is provably dead** vs **provably alive**.

- [ ] **Step 1: Write the failing test — dead session half**

The reliable injection point is the store API (`AcquireLocks(ctx, recs, live)` takes the probe). Add this as a **store** test (it touches `internal/store` → PR rule applies, already covered). Append to `internal/store/locks_test.go`:

```go
// TestAcquireReclaimsDeadSession pins loto-k5el.1 SC2 (dead half): a holder whose
// session pid is provably dead is reclaimed on a peer's acquire, within TTL.
func TestAcquireReclaimsDeadSession(t *testing.T) {
	st := openTestStore(t)              // existing helper
	now := time.Now()
	dead := domain.LockRecord{
		Target: domain.Target{Canonical: tcPathA}, OwnerUUID: tcAlice, SessionUUID: tcAlice,
		Intent: "edit", CreatedAt: now, ExpiresAt: now.Add(time.Hour), // TTL NOT expired
		Host: tcThisHost, PID: 4242, ProcStart: 9999,                   // durable pid, but probe says dead
	}
	mustInsertLock(t, st, dead)         // existing direct-insert helper; see locks_test.go

	// Probe reports pid 4242 dead → liveness-primary reclaim despite live TTL.
	deadProbe := func(host string, pid int, start int64) bool { return false }
	bob := dead
	bob.OwnerUUID, bob.SessionUUID, bob.PID = tcBob, tcBob, 5555
	got, err := st.AcquireLocks(ctx(t), []domain.LockRecord{bob}, deadProbe)
	if err != nil {
		t.Fatalf("bob acquire over dead-session holder must succeed: %v", err)
	}
	if len(got) != 1 || got[0].OwnerUUID != tcBob {
		t.Fatalf("expected bob to hold the reclaimed lock, got %+v", got)
	}
}

// TestAcquireBlocksOnLiveSession pins loto-k5el.1 SC2 (live half): a holder whose
// session pid is alive and TTL unexpired is NOT reclaimed — peer acquire conflicts.
func TestAcquireBlocksOnLiveSession(t *testing.T) {
	st := openTestStore(t)
	now := time.Now()
	live := domain.LockRecord{
		Target: domain.Target{Canonical: tcPathA}, OwnerUUID: tcAlice, SessionUUID: tcAlice,
		Intent: "edit", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Host: tcThisHost, PID: 4242, ProcStart: 9999,
	}
	mustInsertLock(t, st, live)

	liveProbe := func(host string, pid int, start int64) bool { return true }
	bob := live
	bob.OwnerUUID, bob.SessionUUID, bob.PID = tcBob, tcBob, 5555
	_, err := st.AcquireLocks(ctx(t), []domain.LockRecord{bob}, liveProbe)
	var mce *MultiConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("bob acquire over LIVE holder must conflict, got err=%v", err)
	}
}
```

> **Helper note:** `openTestStore`, `mustInsertLock`, `tcAlice`/`tcBob`/`tcPathA`/`tcThisHost`, `ctx(t)` — confirm exact names in `internal/store/testconsts_test.go` and `locks_test.go` and adapt. The store tests already construct `LockRecord`s with these fields (seen in `locks_test.go:322` etc.), so the pattern exists; do not invent new helpers if a direct `tx.Exec` insert helper already serves.

- [ ] **Step 2: Run tests to verify**

Run: `go test ./internal/store/ -run 'TestAcquireReclaimsDeadSession|TestAcquireBlocksOnLiveSession' -v`
Expected: both **PASS** (mechanism exists; tests pin it). A failure here is a real reclaim/conflict bug — debug with `superpowers:systematic-debugging`.

- [ ] **Step 3: Commit**

```bash
git add internal/store/locks_test.go
git commit -m "test(store): pin loto-k5el.1 SC2 — reclaim dead session, block live session"
```

---

### Task 3: domain helpers — `Classify` + `RemainingTTL` (failing test first)

**Files:**
- Test: `internal/domain/staleness_test.go` (append)
- Modify: `internal/domain/staleness.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/domain/staleness_test.go`:

```go
func TestClassifyAndRemainingTTL(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	host := "h"
	aliveProbe := func(string, int, int64) bool { return true }
	deadProbe := func(string, int, int64) bool { return false }

	t.Run("ALIVE: durable pid, probe live, TTL ahead", func(t *testing.T) {
		ec := domain.EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := domain.LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != domain.LivenessAlive {
			t.Errorf("Classify=%v want ALIVE", got)
		}
		if got := ec.RemainingTTL(l); got != time.Hour {
			t.Errorf("RemainingTTL=%v want 1h", got)
		}
	})
	t.Run("DEAD by dead probe, TTL still ahead", func(t *testing.T) {
		ec := domain.EvalContext{Now: now, ThisHost: host, Live: deadProbe}
		l := domain.LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != domain.LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
	})
	t.Run("DEAD by expired TTL even if probe live", func(t *testing.T) {
		ec := domain.EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := domain.LockRecord{ExpiresAt: now.Add(-time.Minute), Host: host, PID: 1, ProcStart: 7}
		if got := ec.Classify(l); got != domain.LivenessDead {
			t.Errorf("Classify=%v want DEAD", got)
		}
		if got := ec.RemainingTTL(l); got != 0 {
			t.Errorf("RemainingTTL=%v want 0 (clamped)", got)
		}
	})
	t.Run("UNKNOWN: PID-0 sentinel, TTL ahead", func(t *testing.T) {
		ec := domain.EvalContext{Now: now, ThisHost: host, Live: aliveProbe}
		l := domain.LockRecord{ExpiresAt: now.Add(time.Hour), Host: host, PID: 0}
		if got := ec.Classify(l); got != domain.LivenessUnknown {
			t.Errorf("Classify=%v want UNKNOWN", got)
		}
	})
	t.Run("Classify=DEAD iff IsStale (invariant I1)", func(t *testing.T) {
		ec := domain.EvalContext{Now: now, ThisHost: host, Live: deadProbe}
		for _, l := range []domain.LockRecord{
			{ExpiresAt: now.Add(-time.Minute), Host: host, PID: 1, ProcStart: 7},
			{ExpiresAt: now.Add(time.Hour), Host: host, PID: 1, ProcStart: 7},
			{ExpiresAt: now.Add(time.Hour), Host: host, PID: 0},
		} {
			if (ec.Classify(l) == domain.LivenessDead) != ec.IsStale(l) {
				t.Errorf("I1 violated for %+v: Classify=%v IsStale=%v", l, ec.Classify(l), ec.IsStale(l))
			}
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/domain/ -run TestClassifyAndRemainingTTL -v`
Expected: FAIL — `ec.Classify` / `domain.LivenessAlive` undefined.

- [ ] **Step 3: Implement the helpers**

Append to `internal/domain/staleness.go`:

```go
// Liveness is the display-tier refinement of IsStale: it splits a non-stale
// lock into ALIVE (owner session probed live) vs UNKNOWN (no durable liveness
// handle — PID-0 sentinel or cross-host — so TTL is the sole authority). DEAD
// is exactly IsStale: TTL backstop fired OR owner provably gone. Surfaced by
// `loto status` so the cause of a lock's verdict is visible (loto-k5el.1).
type Liveness int

const (
	LivenessAlive Liveness = iota
	LivenessDead
	LivenessUnknown
)

func (l Liveness) String() string {
	switch l {
	case LivenessAlive:
		return "alive"
	case LivenessDead:
		return "dead"
	default:
		return "unknown"
	}
}

// Classify returns the display-tier liveness verdict. DEAD ⟺ IsStale (I1).
func (c EvalContext) Classify(l LockRecord) Liveness {
	if c.IsStale(l) {
		return LivenessDead
	}
	if l.PID > 0 && l.Host == c.ThisHost && c.Live != nil {
		// Not stale + durable handle on this host ⟹ probe said alive.
		return LivenessAlive
	}
	return LivenessUnknown
}

// RemainingTTL is the time until the TTL backstop fires, clamped at 0. A live
// durable-PID holder is never TTL-reaped (liveness governs), so this is purely
// informational for ALIVE locks; for UNKNOWN locks it is the self-heal deadline.
func (c EvalContext) RemainingTTL(l LockRecord) time.Duration {
	d := l.ExpiresAt.Sub(c.Now)
	if d < 0 {
		return 0
	}
	return d
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/domain/ -run TestClassifyAndRemainingTTL -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain/staleness.go internal/domain/staleness_test.go
git commit -m "feat(domain): Classify + RemainingTTL display helpers for loto status (loto-k5el.1)"
```

---

### Task 4: `loto status` renders liveness verdict + remaining TTL (failing test first)

**Files:**
- Test: `internal/cli/cmd_status_test.go` (append)
- Modify: `internal/cli/cmd_status.go` (`printStatusLocks`, `statusSingleTarget`)

**Design decision (pin in Task 6 README too):** `status` is **read-only** — it CLASSIFIES via the probe but MUST NOT reap/mutate (invariant I5). Reaping stays in `lock`/`check`/`doctor`. Rationale: `status` is a diagnostic surface; a read command silently deleting rows is surprising and races the op-flock the writers hold. A dead lock shown by `status` is still reclaimed by the next `lock`/`check` — visibility without side effects.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/cmd_status_test.go`:

```go
// TestStatusShowsTTLAndLiveness pins loto-k5el.1 SC3: status reports remaining
// TTL and an owner-liveness verdict per lock.
func TestStatusShowsTTLAndLiveness(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	// Lock with default TTL (30m) and no durable LOTO_PID → liveness UNKNOWN,
	// remaining TTL ~30m.
	t.Setenv("LOTO_PID", "")
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "ttl_remaining=") {
		t.Errorf("status must show ttl_remaining=: %q", s)
	}
	if !strings.Contains(s, "owner=unknown") && !strings.Contains(s, "liveness=unknown") {
		t.Errorf("status must show liveness verdict (unknown for PID-0 sentinel): %q", s)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestStatusShowsTTLAndLiveness -v`
Expected: FAIL — output lacks `ttl_remaining=` / `liveness=`.

- [ ] **Step 3: Modify `printStatusLocks`**

In `internal/cli/cmd_status.go`, change `printStatusLocks` to build an `EvalContext` from the runtime and append the two new fields. Replace the existing per-lock `Fprintf` (the `held_since=… expires_at=… host=… pid=…` line):

```go
func printStatusLocks(stdout io.Writer, rt *runtime, all []domain.LockRecord) {
	if len(all) == 0 {
		fmt.Fprintln(stdout, "✓ no locks")
		return
	}
	fmt.Fprintf(stdout, "✓ locks count=%d\n", len(all))
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	canonicals := make([]string, len(all))
	for i := range all {
		canonicals[i] = all[i].Target.Canonical
	}
	tagsByTarget, _ := rt.Store.ListAliveByTargets(rt.Ctx, canonicals)
	for i := range all {
		l := &all[i]
		fmt.Fprintf(stdout, "✓ target=%s owner=%s intent=%q held_since=%s ttl_remaining=%s liveness=%s host=%s pid=%d\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.Intent,
			l.CreatedAt.UTC().Format(time.RFC3339),
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l),
			l.Host, l.PID)
		render.EmitTagRows(stdout, tagsByTarget[l.Target.Canonical])
	}
}

// fmtTTL renders a remaining-TTL duration deterministically (whole seconds,
// "0s" when the backstop has fired). Avoids time.Duration's variable-precision
// String so status output is byte-stable for golden tests (design.md).
func fmtTTL(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d.Round(time.Second)/time.Second))
}
```

> Keep `expires_at` OUT of the new line (replaced by `ttl_remaining`) — remaining TTL is the actionable signal per the bead; the absolute timestamp added noise. If a downstream golden test asserts the old `expires_at=` substring, update that golden in the same commit.

- [ ] **Step 4: Mirror into `statusSingleTarget`**

In the same file, update the per-holder line in `statusSingleTarget` (currently `✗ holder … expires_at=…`) to use the same `ec` + `fmtTTL` + `Classify`. Build `ec` once at the top of `statusSingleTarget` after `ListLocks`:

```go
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}
	...
	for i := range overlapping {
		l := &overlapping[i]
		fmt.Fprintf(w, "✗ holder target=%s owner=%s intent=%q ttl_remaining=%s liveness=%s\n",
			relPath(l.Target.Canonical), l.OwnerUUID, l.Intent,
			fmtTTL(ec.RemainingTTL(*l)), ec.Classify(*l))
	}
```

> `statusSingleTarget` currently takes `(w io.Writer, rt *runtime, t domain.Target)` — `rt` is in scope, so `rt.Host`/`rt.liveProbe()` are available. No signature change.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run TestStatusShowsTTLAndLiveness -v`
Expected: PASS.

- [ ] **Step 6: Run the full cli + render suite for golden-test fallout**

Run: `go test ./internal/cli/ ./internal/render/ -v 2>&1 | tail -40`
Expected: PASS. If a golden/help/contract test (`help_golden_test.go`, `output_glyphs_test.go`, `acceptance_test.go`) trips on the changed status line, update the golden to the new format in this commit — the new fields are intentional.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/cmd_status.go internal/cli/cmd_status_test.go
git commit -m "feat(cli): loto status shows ttl_remaining + liveness verdict (loto-k5el.1 SC3)"
```

---

### Task 5: `loto status` verdict agrees with the reaper (cross-check test)

**Files:**
- Test: `internal/cli/cmd_status_test.go` (append)

Pins invariant I3 at the CLI seam: a lock `status` reports `liveness=dead` is one a peer `lock` reclaims with no doctor — so `status`'s verdict is trustworthy, not cosmetic.

- [ ] **Step 1: Write the test**

```go
// TestStatusDeadVerdictMatchesReclaim pins I3: a lock status calls `dead`
// (expired TTL) is reclaimed by a peer acquire with no doctor run.
func TestStatusDeadVerdictMatchesReclaim(t *testing.T) {
	withTempProject(t)
	t.Setenv("LOTO_PID", "")
	pinAgentAs(t, "alice")
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest, "--ttl", "-1s"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	var st bytes.Buffer
	Run([]string{tcCmdStatus}, &st, &bytes.Buffer{})
	if !strings.Contains(st.String(), "liveness=dead") && !strings.Contains(st.String(), "ttl_remaining=0s") {
		t.Fatalf("status should flag expired lock dead / 0s: %q", st.String())
	}
	pinAgentAs(t, "bob")
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob should reclaim the dead-verdict lock with no doctor")
	}
}
```

- [ ] **Step 2: Run to verify**

Run: `go test ./internal/cli/ -run TestStatusDeadVerdictMatchesReclaim -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_status_test.go
git commit -m "test(cli): status dead-verdict matches peer reclaim (loto-k5el.1 I3)"
```

---

### Task 6: Document the mid-edit-expiry policy + liveness columns (README)

**Files:**
- Modify: `README.md`

The design guidance demands a decided, documented policy for "the backstop fires while a live-but-unprobeable agent is mid-edit." The code already encodes the answer; this task makes it explicit.

**The decided policy (lift-from-Jeff-adapted-to-local):**
1. **Liveness-primary means the hazard rarely materializes.** A durable-PID (LOTO_PID) holder whose session is alive is `ALIVE` and is **never** TTL-reaped — `IsStale` returns false because the probe says alive, regardless of `expires_at`. So a real, live Claude session editing for hours past a 30m TTL is *not* stolen. (This is the key divergence from pure-TTL leases the bead calls out.)
2. **The residual hazard is the UNKNOWN holder** (PID-0 sentinel: no LOTO_PID, e.g. bare-shell/cron/hook-misconfig). For those, TTL is the sole authority and *can* expire mid-edit. Policy: **TTL default is generous (30m) and renewable** (Task 7 `loto lock --renew` re-stamps `expires_at`). A wrapper/long task re-locks to extend. We do **not** add a grace period or a "warn don't steal" mode — the PID-0 case is already the degraded path, flagged at acquire by `degradedPidWarning()`; the fix is to set LOTO_PID (promoting the holder to ALIVE), not to soften the backstop.
3. **`status` is read-only** (invariant I5): it shows `liveness=` and `ttl_remaining=` so an operator sees an imminent expiry, but never reaps. Reaping is `lock`/`check`/`doctor`.

- [ ] **Step 1: Add a "Self-healing locks" subsection to README**

Under the existing `## design invariants` / TTL discussion (around the "record-tier carve-out" lines), add:

```markdown
### Self-healing locks (liveness-primary, TTL backstop)

A lock frees the instant its owner is provably gone — no manual `loto doctor`:

- **Liveness-primary.** Each lock stamps the owning **session** pid (`LOTO_PID`,
  exported by the SessionStart hook — NOT the one-shot CLI pid) plus the
  process start-time (`proc_start`, defeats PID reuse). On any `loto lock` or
  `loto check`, a holder whose session is provably dead is reclaimed in-line.
  A clean session exit releases eagerly via the SessionEnd hook.
- **TTL backstop.** `--ttl` (default 30m) bounds the residual cases liveness
  can't cover: no durable `LOTO_PID` (bare shell / cron), cross-host rows, or a
  store that crossed a host reboot. Generous by design — the backstop, not the
  path.
- **Mid-edit expiry.** A live session (durable PID, probe alive) is NEVER
  TTL-reaped, so a long edit past the TTL is safe. Only an UNKNOWN holder
  (PID-0 sentinel) can expire mid-edit; extend it with `loto lock --renew`, or
  fix the SessionStart hook to export `LOTO_PID` (promoting it to alive). loto
  warns at acquire when liveness has degraded to TTL-only.
- **`loto status`** shows `liveness=alive|dead|unknown` and `ttl_remaining=` per
  lock so the cause of every verdict is visible. status is read-only — it never
  reaps.
```

- [ ] **Step 2: Verify the README contract test still passes**

Run: `go test ./internal/cli/ -run 'Help|Contract|Readme|README' -v 2>&1 | tail -20`
Expected: PASS (if any test asserts README content, align it).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(readme): document liveness-primary self-heal + mid-edit-expiry policy (loto-k5el.1)"
```

---

### Task 7: `loto lock --renew` — re-stamp TTL on an owned lock (failing test first)

**Files:**
- Test: `internal/cli/cmd_lock_test.go` (append)
- Modify: `internal/cli/cmd_lock.go`

This is the renewal escape hatch the mid-edit policy promises. **No store-API change is needed** — `insertOrRefreshLock` already `ON CONFLICT DO UPDATE`s `expires_at` *when `owner_uuid` matches* (see `locks_acquire.go:287`). `loto lock <target> --renew` is just a normal acquire by the same owner with a fresh TTL: the upsert refreshes the row. `--renew` mainly changes semantics on **conflict** — without it, re-locking your own held target already succeeds (owner match), so `--renew` is largely a clarity alias. **Scope decision:** implement `--renew` as a flag that (a) is documented intent, (b) errors clearly if the target is NOT currently held by this owner (so "renew" can't silently create a brand-new lock). Keep it minimal.

- [ ] **Step 1: Write the failing test**

```go
// TestLockRenewExtendsOwnHeldLock pins the renewal escape hatch (loto-k5el.1):
// `loto lock --renew` on an owned target pushes expires_at forward.
func TestLockRenewExtendsOwnHeldLock(t *testing.T) {
	withTempProject(t)
	t.Setenv("LOTO_PID", "")
	pinAgent(t)
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest, "--ttl", "5m"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("initial lock failed")
	}
	var before bytes.Buffer
	Run([]string{tcCmdStatus}, &before, &bytes.Buffer{})

	// Renew with a longer TTL.
	var out, errb bytes.Buffer
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest, "--renew", "--ttl", "60m"},
		&out, &errb); code != 0 {
		t.Fatalf("renew should succeed on owned lock: exit, out=%q err=%q", out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "✓") {
		t.Errorf("renew should report success: %q", out.String())
	}
}

// TestLockRenewFailsOnUnheldTarget: --renew must not silently create a fresh lock.
func TestLockRenewFailsOnUnheldTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errb bytes.Buffer
	code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest, "--renew"}, &out, &errb)
	if code == 0 {
		t.Errorf("--renew on an unheld target must fail, got success: %q", out.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run 'TestLockRenew' -v`
Expected: FAIL — `--renew` flag unknown (exit 2).

- [ ] **Step 3: Implement the flag**

In `internal/cli/cmd_lock.go`, add the flag next to `--ttl` and a pre-acquire ownership check:

```go
	ttl := fs.Duration("ttl", 30*time.Minute, "lock TTL")
	renew := fs.Bool("renew", false, "re-stamp TTL on a lock you already hold; errors if you don't hold it")
```

Before `acquireBatch`, when `*renew` is set, verify the owner holds every target (use `rt.Store.ListLocks` filtered by `rt.Agent.UUID` and target canonical). If any target is not held by this owner, emit a glyph-led error and return 2:

```go
	if *renew {
		if code := ensureHeldByMe(rt, targets, stderr); code != 0 {
			return code
		}
	}
	return acquireBatch(rt, targets, *intent, *ttl, rt.liveProbe(), stdout, stderr)
```

Add the helper (same file):

```go
// ensureHeldByMe verifies every target is currently locked by this owner, so
// `--renew` extends an existing lease rather than silently minting a new one.
func ensureHeldByMe(rt *runtime, targets []domain.Target, stderr io.Writer) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	held := map[string]bool{}
	for i := range all {
		if all[i].OwnerUUID == rt.Agent.UUID {
			held[all[i].Target.Canonical] = true
		}
	}
	var missing []string
	for _, t := range targets {
		if !held[t.Canonical] {
			missing = append(missing, relPath(t.Canonical))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintf(stderr, "✗ renew count=%d not-held-by-you\n", len(missing))
		for _, m := range missing {
			fmt.Fprintf(stderr, "✗ target=%s reason=not-held\n", m)
		}
		return 2
	}
	return 0
}
```

> Add `"sort"` to the import block if absent. `relPath` and `domain.Target` are already used in this package.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/cli/ -run 'TestLockRenew' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cmd_lock.go internal/cli/cmd_lock_test.go
git commit -m "feat(cli): loto lock --renew re-stamps TTL on owned lock (loto-k5el.1)"
```

---

### Task 8: Full verification + PR

**Files:** none (verification gate)

- [ ] **Step 1: Build, vet, lint, full test**

```bash
go build ./... && go vet ./... && golangci-lint run ./... && go test ./...
```
Expected: all green. (Local macOS has no `-race`; that runs on CI. Per `.claude/rules/workflow.md`, do NOT treat local pass as sufficient for the store-touching change — CI's linux `-race` is the gate.)

> phantom-lint caveat (workflow.md): golangci can flag findings in `.claude/worktrees/agent-*` copies. If a finding's path is outside this worktree's real `internal/`, verify against the real source; `golangci-lint cache clean` if stale.

- [ ] **Step 2: Push the branch and open the PR**

This change touches `internal/store/locks_test.go` and `internal/domain/*` + `internal/cli/*` → **must** go through a PR (store-touch rule). The plan was authored on `plan/loto-k5el.1`; implementation commits land on a `impl/loto-k5el.1` branch off main (or continue on a dedicated impl branch — do NOT push impl to main directly).

```bash
git push -u origin <impl-branch>
gh pr create --title "feat(loto): TTL self-heal surfacing + renew (loto-k5el.1)" \
  --body "Closes via loto-k5el.1. Liveness-primary self-heal already shipped (loto-t1tq/j1bo/kwlp/9t0q); this PR closes SC3 (status shows ttl_remaining + liveness), adds the --renew escape hatch, documents the mid-edit-expiry policy, and pins SC1/SC2 with tests. Store-touch → CI -race gate."
```

- [ ] **Step 3: Update bead + close on merge**

```bash
bd update loto-k5el.1 --status in_progress   # while PR open
# on merge:
bd close loto-k5el.1 --reason "TTL self-heal surfacing + renew shipped; SC1-3 pinned"
```

---

## Open Questions (for dk)

1. **Is loto-k5el.1 effectively already done?** The grounding pass found liveness-primary self-heal, TTL backstop, the CLI-PID-trap fix, PID-reuse defense, lazy reaping in `lock`/`check`, and SessionEnd eager-release all shipped (loto-t1tq, j1bo, kwlp, 9t0q, vtg6, l3as). The **only unmet Success Criterion is SC3** (`status` showing remaining TTL). If you consider SC3 the whole remaining scope, this plan over-delivers (it also adds `--renew` + docs). **Want me to trim to just Task 3–5 (status TTL/liveness) and close the bead, deferring `--renew` to a follow-up?**

2. **`--renew` value vs. cost.** Because `insertOrRefreshLock` already refreshes `expires_at` for the owner on a plain re-`lock`, `--renew` is largely a guardrail (it errors if you don't already hold the target) rather than new capability. Is the explicit `--renew` worth the surface area, or is "just re-lock to extend" sufficient (drop Task 7)?

3. **status read-only vs. reap-on-read.** I decided `status` classifies but never reaps (invariant I5) — a read command mutating rows is surprising and races the writers' op-flock. Acceptable, or do you want `status` to reap dead rows so the table self-cleans even when nobody acquires? (Would require taking the op-flock in a read path — I'd advise against.)

4. **`expires_at` → `ttl_remaining` swap in status output.** Task 4 replaces the absolute `expires_at=<RFC3339>` field with `ttl_remaining=<Ns>` (more actionable per the bead). If any external consumer greps `expires_at` from `status`, that's a breaking output change. Keep both, or swap? (design.md favors the actionable signal; I swapped.)

5. **Mid-edit policy: no grace period.** I deliberately did NOT add a grace period or "warn-don't-steal" mode for backstop expiry, because the only holder that can expire mid-edit is the UNKNOWN (PID-0) case, whose real fix is exporting `LOTO_PID`. Jeff's mcp_agent_mail uses grace/renewal because it's networked and can't probe liveness; loto can, so the grace period is redundant here. Agree, or do you want a grace window on the PID-0 path as defense-in-depth?

6. **Shared/exclusive (loto-k5el.2) interaction.** This plan is exclusive-only (matches today's binary locks). The sibling bead loto-k5el.2 adds shared/exclusive + downgrade. The `Classify`/`ttl_remaining` surfacing here is mode-agnostic and should compose, but I did not design for shared-mode TTL semantics (e.g. does a shared lock's TTL behave differently?). Flagging so loto-k5el.2's plan accounts for it.
