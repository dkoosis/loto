# treehouse — prior-art analysis (feeds the `wt-*` fork decision)

> **Purpose.** dk asked "what do you think of this approach?" re
> [`github.com/kunchenguid/treehouse`](https://github.com/kunchenguid/treehouse).
> This is the studied answer, **verified against treehouse's source** (not just
> its README — the README undersells the concurrency story and I initially got
> it wrong from the README alone). It feeds the open `wt-*` graduation fork
> (`docs/wt-harness-migration-brief.md` §5/§8 — fork A bash vs fork B `loto wt`).
> Written 2026-07-05.

---

## 0. TL;DR

treehouse is **not** a competitor to loto-core. It's a competitor — a good,
better-engineered one — to the **`wt-*` harness loto is absorbing**. It solves
**tree isolation** (one agent ↔ one warm worktree from a pool). loto-core solves
**write coordination** (two agents, one file, no clobber). Adjacent layers. They
**compose**; they don't compete.

Verdict: steal three ideas into fork B, evaluate a fourth, leave two behind.
Do **not** adopt wholesale.

> **⟶ Decision home (2026-07-06):** `~/Projects/dk/Project/loto/specs/worktree-strategy-design.md` single-sources the call. treehouse stays **DEFERRED** behind the gate-first re-measure (decision **D4** there) — wrap it only if a Layer-2 throughput tier still earns a slot post-gate; do not port the nine scripts. The steal/evaluate/leave verdict below stands as the source input.

---

## 1. The layer model (the part that's easy to conflate)

```
Layer 3  TASK ROUTING     which agent gets which job       /team fleet · isolation:worktree (trixi-5qh5)
Layer 2  TREE ISOLATION   one agent ↔ one worktree     ┌─ treehouse pool
                                                        └─ loto wt-* harness   ← SAME layer; the real comparison
Layer 1  WRITE COORD      two agents, one file, no clobber   loto CORE (file leases)   ← loto-fs84 lives HERE
Layer 0  GIT              worktrees, branches, reset, fetch
```

The load-bearing insight: **treehouse sits at Layer 2, loto-core at Layer 1.**
When you compare treehouse to *loto the file-lock tool* you get confused,
because they aren't the same thing. Compare it to the **`wt-*` harness** and it
clicks — that's the apples-to-apples.

Two consequences fall out:

- **fs84 (subagents sharing one handle, same-file edits) is a Layer-1 problem.**
  treehouse does not address it and cannot — its answer is "don't share a tree,
  take your own from the pool," which is a Layer-2 move. That answer is only
  valid *if the layer below it (per-agent tree assignment) actually fires* —
  which is exactly the `isolation:worktree` silently-ignored bug (`trixi-5qh5`).
  So treehouse assumes away the thing that's currently broken.
- **loto's north star and treehouse philosophically disagree — correctly, per
  layer.** NORTH_STAR: "Worktrees just delay the issue until merge." True at
  Layer 2. treehouse's whole bet is Layer-2 isolation + aggressive reset. loto's
  bet is that you *also* need Layer 1 because isolation only defers the merge
  collision. **Both right.** treehouse-style Layer-2 pooling *underneath* loto
  Layer-1 locking is a strictly stronger stack than either alone.

---

## 2. What the code actually does (verified, not README-inferred)

Read: `internal/pool/pool.go`, `internal/pool/lock_unix.go`,
`internal/pool/state.go`, `internal/process/detect.go`.

| Claim | Verified finding |
|---|---|
| **Acquisition race** | ✓ **GUARDED.** `WithStateLock` takes an exclusive `syscall.Flock(LOCK_EX)` over pool state; the whole scan→select→`markAcquired` runs atomically inside it. Two concurrent `get`s cannot double-grab. *(My first-pass "unguarded TOCTOU" read from the README was wrong — the README says "no explicit concurrency guarantees" but the code guards it.)* |
| **`--lease` vs process-scan** | ✓ Durable lease is recorded in pool state and is the *real* reservation (survives no-process, cleared only by `return`). Process-scan is the best-effort hint layer. This is loto's own hard-won `k5el` lesson ("stamp = detection-only beacon, not preventive") — treehouse arrived at it independently. |
| **In-use detection** | ⚠ `FindProcessesInWorktree` scans **all** procs and marks in-use if a proc's **cwd** is at/under the worktree. Fragile as a *gate*: a proc that `cd`s out (or a build subprocess working in a tmpdir) reads as not-in-use. Fine as a *hint* — and treehouse treats it as exactly that, with `--lease` as the guarantee. Don't lean on it. |
| **git work under the lock** | ⚠ `git fetch` is outside the lock (good). But per-candidate `ResetWorktree` appears to run **inside** the `WithStateLock` critical section during acquire → holds the exclusive pool lock across a `git reset` shell-out → serializes all concurrent acquires behind reset time. Correctness fine; **throughput smell.** Confirm against source before copying the pattern. |
| **Return / dirty state** | ✗ `Release` calls `git.ResetWorktree` to default **unconditionally — discards uncommitted changes, no stash, no archive.** Genuine footgun for agents that forget to commit. loto's `wt-discard` already does this better: refuses a dirty tree without `--archive-patch`/`--force-with-confirmation`. |
| **Detached HEAD** | ℹ Worktrees run detached, reset to whichever of local/remote default is further ahead. Deletes the branch-name-collision problem entirely — no `fix/<id>` taxonomy, no `gh-poi`, no `wt-gc` branch deletion. Elegant, but trades away bead↔branch traceability (`branch.<name>.bead`, the `fix/<id>` convention wt-status/wt-land rely on). |

---

## 3. Steal / evaluate / leave

**Steal into fork B (`loto wt`):**

1. **Warm reusable pool** instead of create-per-bead throwaway. The `wt-*`
   harness eats a cold worktree (lost build cache + deps) every bead; treehouse's
   whole thesis is reuse-a-warm-tree. This is the single best idea to port and it
   directly attacks the "why are your agents slow" opener in the migration brief.
2. **Durable lease as the primitive, process-scan as hint only.** You already
   believe this (`k5el`); treehouse is independent confirmation and a clean
   reference implementation of the split.
3. **Exclusive flock over pool-state across scan+reserve.** The `wt-*` bash
   scripts scan-then-act (`wt-status` → `wt-gc`) *without* this atomicity;
   `lint-locked` proves the team already reaches for a mutex when it matters.
   Adopt `WithStateLock`'s pattern for pool mutation.

**Evaluate (not free):**

4. **Detached-HEAD / no-branch model.** Deletes `gh-poi` + `wt-gc` branch
   management wholesale — real complexity reduction. But it trades away the
   bead↔branch traceability loto's land/close flow depends on. Worth a spike;
   not a drop-in swap.

**Leave behind:**

- **Unconditional dirty-discard on return.** Keep `wt-discard`'s archive guard.
- **cwd-process-scan as a gate.** Hint only; the lease is the gate.

---

## 4. Draft bead (file with `bd` — not creatable from this env)

```
title:  wt-* fork B: adopt treehouse-style warm pool + durable-lease primitive
type:   task    priority: P2    requires_plan: true
body:
  Prior-art analysis of github.com/kunchenguid/treehouse (docs/treehouse-prior-art.md)
  feeds the wt-* graduation fork (docs/wt-harness-migration-brief.md §5).
  Adopt into `loto wt`: (1) warm reusable worktree pool vs create-per-bead;
  (2) durable lease as reservation primitive, process-scan as hint only;
  (3) exclusive flock over pool-state across scan+reserve.
  Spike separately: detached-HEAD/no-branch model (deletes gh-poi/wt-gc branch
  mgmt but drops branch.<name>.bead traceability — evaluate the trade).
  Do NOT copy: unconditional dirty-discard on return (keep wt-discard's archive
  guard); cwd-process-scan as a gate.
  Note: does NOT address fs84 (Layer-1 same-file coord) — treehouse assumes
  Layer-2 per-agent tree assignment, which is trixi-5qh5's broken substrate.
Closes: none
```

---

## 5. Addendum — primary-session deltas (2026-07-05)

Three facts the cloud session above didn't have; they change §3/§4's framing
from "steal into fork B" to "resolve the fork first."

1. **Fork B contradicts a newer settled decision.** The wt-graduation call
   (2026-06-01, brief §0) predates the `/team` one-tree pivot (2026-06-19,
   loto-9sro lanes: worktrees *retired* from the fleet harness for cause —
   orphan detritus, cold starts, phantom lint, commit churn; dk reaffirmed
   2026-07-04). A native `loto wt` verb set would build into loto-core the
   model its own NORTH_STAR argues against. Recommendation: **kill fork B**;
   loto stays Layer 1 (locks · claims · gate · doctor). If a Layer-2 tier
   survives the re-measure below, **wrap treehouse** (its concurrency core is
   verified sound, §2) instead of porting nine bash scripts into Go — patches
   needed: archive-before-reset (keep `wt-discard`'s guard), lease TTLs aligned
   with loto's, and the lint exclusion in (2). `wt-land`'s land-and-close flow
   stays regardless — that's bead lifecycle, not tree lifecycle.

2. **Phantom-lint scar (transcript audit, ~10 sessions of drag).** Pooled trees
   share the Go module path exactly like `.claude/worktrees/agent-*` did:
   `./...` lints/tests the duplicate copy, and stale golangci caches keep
   findings alive after dirs vanish (fixed for `.claude/` in #60). Any pool dir
   must be excluded from lint/test globs **on day one** — treehouse's model
   doesn't know about this failure class.

3. **Gate-first sequencing.** The only real clobber in ~69 audited sessions
   (kuv.10, ccp-l1nf) is exactly what the spec'd Layer-1 stack closes
   (`gate-design.md`: 7af9 claims → agent_id check-only gate → ebkc TTL;
   plans written 2026-07-05, awaiting dk approval). Once it ships, physical
   isolation stops being a *safety* argument and becomes a *throughput* one
   (one-tree's honest remaining cost: no concurrent `make check`). Ship the
   gate, re-measure, and only then decide whether any pool tier is worth
   wrapping. §4's draft bead should not be filed until that re-measure.

---

*Cross-refs: `docs/wt-harness-migration-brief.md` (the graduation this feeds),
`docs/NORTH_STAR.md` ("worktrees just delay the issue until merge"),
`docs/decisions/0002-canonical-base.md` (shared-base coordination), boot beads
`loto-fs84`/`loto-k5el`, trixi `trixi-5qh5`,
`~/Projects/dk/Project/Inquiries/loto-identity-lock-model/gate-design.md`
(the Layer-1 stack that resequences this decision).*
