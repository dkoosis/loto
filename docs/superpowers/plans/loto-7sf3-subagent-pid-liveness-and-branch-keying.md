# loto-7sf3 — subagent pid=0 liveness + branch-blind gate (shared-tree fleet)

status: plan-for-approval · author: plan agent (unattended) · date: 2026-06-12
bead: loto-7sf3 · requires_review: yes · store-adjacent → ship via PR, never direct-to-main

## Problem

During a shared-tree fleet run (N background CC subagents, one working tree, loto
as the only write-serializer), two guarantees went soft:

1. **liveness=unknown / pid=0 on self-held exclusive locks.** Subagents' locks
   carried the PID-0 sentinel, so liveness degraded to TTL-only — silently,
   because the degrade warning is also gated on an env var the subagents lacked.
2. **The check gate did not hard-refuse a second editor.** Two agents edited
   `oauth.go` without serialization, and one escaped the shared-branch contract
   via raw `git checkout` in Bash.

These are one defect plus two design questions, not three independent bugs.

## Evidence (verified against source, 2026-06-12, branch team/impl-20260612-1818 @ f41c88d)

- `internal/cli/stamppid.go:26-35` — `stampPID()` reads only `LOTO_PID`. Unset →
  `(0, pidUnset)` = the exact pid=0 signature. There is no fallback.
- `.claude/settings.json:11,15` — `LOTO_PID` is exported by a SessionStart hook
  doing a PPID ancestor-walk for a process named `claude`, appended to
  `$CLAUDE_ENV`. Background subagents in a fleet do not get this hook run for
  themselves and evidently do not inherit the export (observed pid=0). Same for
  the `LOTO_AGENT_ID` hook on line 7.
- `internal/cli/stamppid.go:42-56` — `degradedPidWarning()` returns "" when
  `LOTO_AGENT_ID` is unset. A subagent missing **both** vars degrades with no
  warning at all — explains why the agents only noticed via later `check` output.
- `internal/cli/cmd_check.go:159-166` — `blocking := ec.Classify(*l) == LivenessAlive`.
  A PID-0 exclusive holder classifies UNKNOWN (`internal/domain/staleness.go:78-87`)
  → advisory row, exit 0. **This is the cascade:** defect 1 (pid=0) demotes every
  fleet lock to UNKNOWN, so the PreToolUse gate's hard refusal never fires, so two
  agents could both edit `oauth.go`. The advisory-on-UNKNOWN behavior itself is a
  deliberate prior call (loto-k5el.2 binding correction 4, loto-9t0q,
  `TestCheck_UnknownExclusivePeerWarns` in `cmd_check_shared_test.go:50`).
- `internal/cli/cmd_check.go` — zero references to `branch`. Check keys on
  canonical path only.
- `internal/store/locks_acquire.go:299-313` — `insertOrRefreshLock` persists
  `l.Branch`; `internal/store/locks.go:198` scans it back. But
  `buildLockRecords` (`internal/cli/cmd_lock.go:186-213`) **never sets `Branch`**
  and no other production code does (`rg '\.Branch|Branch:'` over `internal/`
  hits only store + `domain/records.go:24`). The branch column is dead — always
  written as `""`. The bead's "written but never consulted" understates it: it is
  never even populated.
- `docs/NORTH_STAR.md:13-17` — non-goal: "Enforced consistency. Loto assumes a
  cooperative team and does not prevent a process from changing permissions and
  directly writing to files." Raw `git checkout` via Bash is the same class of
  escape.
- Prior art: `docs/superpowers/plans/loto-k5el.1-ttl-self-heal.md` (~line 623):
  "the fix is to set LOTO_PID, not to soften the backstop." This plan follows
  that line: restore durable pids; do not weaken or re-tier the UNKNOWN policy.

## Root cause

`LOTO_PID` delivery depends on a CC SessionStart hook + `$CLAUDE_ENV`
propagation. Background subagents sit outside that delivery path, so every fleet
lock is PID-0 → UNKNOWN → the gate's hard-block tier is unreachable exactly in
the scenario the fleet model needs it. The branch divergence is a separate,
mostly-out-of-star concern (see Design call 3).

## Design options

### 1. Restoring durable pids for subagents

| Option | Sketch | Verdict |
|---|---|---|
| A. CLI-side ancestor-walk fallback | When `LOTO_PID` is unset/invalid, loto itself walks the PPID chain (≤12 hops) for comm `claude` and stamps that pid | **Recommended.** Removes the env-delivery dependency entirely; works for subagents, bare CC Bash, and any future harness shape. The walked-to pid is the long-lived session process — same value the hook computes, same durability semantics. Subagents share the parent CC process, so the ancestor pid is exactly the right liveness handle. |
| B. Fix env propagation in the cc-plugins fleet harness | Make /team export LOTO_PID into subagent Bash env | Out of this repo (bead: "Do NOT touch cc-plugins"). Also fragile: re-solves delivery per harness. File a cc-plugins bead as follow-up regardless — hook stays the fast path. |
| C. Accept TTL-only for subagents, document | No code | Rejected: voids fast-reclaim AND the hard gate for the fleet, the primary loto use case. |

Implementation note for A: no `ps` exec. darwin already has
`unix.SysctlKinfoProc` plumbing (`procstart_darwin.go`) — `KinfoProc` exposes
`Eproc.Ppid` and `Proc.P_comm` ([16]byte, "claude" fits). linux: parse
`/proc/<pid>/stat` (comm in parens, ppid = field 4). other: no-op → sentinel.
Same `//go:build` split as `procstart_*.go`. Walk starts at `os.Getppid()`
(loto ← shell ← … ← claude). Returned pid feeds the existing
`procStart(pid)` start-time stamp, so PID-reuse defense (loto-kwlp) holds.

New `pidSource` value `pidAncestor` — treated as durable everywhere, but
distinguishable for warning copy and tests. `LOTO_PID` still wins when set
(explicit beats inferred; keeps the hook authoritative when present).

### 2. The gate's UNKNOWN-exclusive-peer policy

Leave it. UNKNOWN→advisory is a settled call (loto-k5el.2 T8, loto-9t0q) that
exists for cross-host and bare-shell holders; hard-blocking on UNKNOWN would
make any hook misconfig a fleet-wide deadlock requiring `unlock --force`. With
Option A, fleet holders classify ALIVE and the hard block works as designed —
fix the input, not the tiering. (Open question 2 offers dk a narrower
alternative if he wants belt-and-suspenders.)

### 3. Branch: keying, metadata, or delete

- **Keying check/Conflicts on (path, branch): rejected.** In a shared tree, two
  agents on different branches editing the same path are editing the *same
  bytes* — branch-keying would let both pass, i.e. it weakens the gate in
  precisely the failure mode observed. Worktrees already get distinct canonical
  paths, so branch adds nothing there either. Path-only keying is correct;
  pin it with a test.
- **Populate `Branch` at lock time + surface drift as advisory: recommended.**
  Stamp `git branch --show-current` (best-effort, "" on error/detached) in
  `buildLockRecords`. In `check` conflict rows, when the blocker's recorded
  branch differs from the tree's current branch, append `branch=<theirs>`
  (holder context) — evidence for post-mortems like 1r4's divergence, zero
  enforcement. This honors the north-star line: observe and report, don't
  prevent.
- **Delete the column: viable fallback** if dk prefers less surface — it has
  never held data, so dropping is migration-free in practice. Default to
  populate; it directly serves the fleet-debugging need this bead came from.
- **Bash `git checkout` escape: out-of-star for loto core.** NORTH_STAR
  non-goals exclude enforced consistency. The PreToolUse gate firing only on
  Edit/Write (not Bash git) is a cc-plugins hook property; if dk wants a Bash
  matcher that runs `loto check` on git-checkout/branch commands, that is a
  cc-plugins bead. Loto's contribution is the branch metadata (above) that
  makes such a hook — and post-hoc forensics — possible.

## Recommended approach (summary)

1. `stampPID()` falls back to an in-process ancestor walk for the `claude`
   session pid when `LOTO_PID` is absent/invalid (`pidAncestor`). Hook value
   still wins when present.
2. Widen `degradedPidWarning()`'s "inside a Claude session" detection to
   `LOTO_AGENT_ID` **or** `CLAUDECODE` env (CC sets `CLAUDECODE=1` in Bash), so
   a subagent that still ends up at the sentinel degrades *loudly*.
3. Populate `LockRecord.Branch` at acquire; render holder-branch on check
   conflict rows when it differs from the current branch. No keying change.
4. Pin path-only keying with a test: same path, two holders with different
   recorded branches → gate behavior unchanged by branch.
5. Docs: README liveness section gains a "background subagents" paragraph;
   note the cc-plugins follow-ups (env propagation, Bash-git gate) as beads,
   not code here.

## Open questions for dk

1. **Bless the ancestor-walk fallback as a peer of the hook?** It makes loto
   self-sufficient for pid discovery but adds per-acquire process-table reads
   and a hardcoded comm match (`claude`). Alternative: keep loto pure and fix
   delivery in cc-plugins only — but that re-breaks for every new harness.
2. **Same-host UNKNOWN exclusive peer: stay advisory?** Recommendation is yes
   (settled by loto-k5el.2/loto-9t0q). The narrower alternative — hard-block
   only when `holder.Host == thisHost` and liveness is UNKNOWN — closes the
   residual window if pid discovery ever fails again, at the cost of
   `unlock --force` friction for every bare-shell lock on the same machine,
   and it flips `TestCheck_UnknownExclusivePeerWarns`. Needs dk's call.
3. **Branch column: populate + advisory display (recommended) or delete?**
   It has never held data, so both are cheap now; deciding later costs a
   migration mindset once real rows carry it.
4. **Where exactly is the north-star line for the Bash escape?** Plan treats
   raw `git checkout` interception as out-of-star (cooperative-team non-goal),
   with loto supplying metadata only. Confirm, so the cc-plugins bead can be
   scoped (or explicitly declined).
5. **comm match list.** Walk matches `claude` only (mirrors the hook). Should
   it be extensible (`LOTO_SESSION_COMM` override) for non-CC harnesses, or is
   YAGNI the right call until a second harness exists?

## Implementation outline (post-approval)

All steps TDD: failing test first. Tests stdlib-only, in-package (`package cli`
/ `package domain`) per workflow.md — no testify, no black-box `_test` self-import.

**Task 1 — ancestor-walk primitive.**
- New `internal/cli/ancestorpid_darwin.go`, `ancestorpid_linux.go`,
  `ancestorpid_other.go` (build-tag split mirroring `procstart_*.go`).
- Core: `findSessionPid(start int, lookup procLookup) (int, bool)` where
  `procLookup func(pid int) (ppid int, comm string, ok bool)` — the walk logic
  is platform-independent and unit-testable with a fake process table; only
  `procLookup` is per-OS. Depth cap 12, stop at pid ≤ 1, match comm `claude`.
- `internal/cli/ancestorpid_test.go`: fake-table cases — found at depth 1/3,
  not found (→ false), cycle/garbage ppid (→ false), depth cap honored.

**Task 2 — wire into stampPID.**
- `stamppid.go`: add `pidAncestor` source; `stampPID()` order: valid `LOTO_PID`
  → `pidDurable`; else walk → `pidAncestor`; else sentinel with existing
  unset/invalid sources. Package-level `var sessionPidFallback = …` seam (same
  pattern as `killFn` in `pid_unix.go:10`) so `stamppid_test.go` stubs it.
- `cmd_lock.go:186-196`: `buildLockRecords` treats `pidAncestor` like
  `pidDurable` (procStart stamped). Suggest folding to `src.durable()` helper.
- Tests: extend `TestStampPID` (env unset + stub finds ancestor → that pid,
  `pidAncestor`; env set wins over stub; stub miss → sentinel). Update
  existing subtests that assume unset → sentinel to pin the stub to miss.

**Task 3 — loud degrade for subagents.**
- `degradedPidWarning()`: claude-session detection = `LOTO_AGENT_ID != "" ||
  CLAUDECODE != ""`; copy mentions the fallback also failed. Only fires when
  `stampPID()` lands on the sentinel (unchanged contract otherwise).
- Tests: extend `TestDegradedPidWarning` (CLAUDECODE-only env warns; bare
  shell still silent; ancestor-resolved pid silent).

**Task 4 — populate Branch + advisory render.**
- `cmd_lock.go` `buildLockRecords`: add `branch` param; caller resolves via
  `git branch --show-current` with `gitTimeout` + `cmd.Dir = repoTop`
  (best-effort: "" on error/detached/non-repo — never fails the lock).
- `cmd_check.go` `printCheckConflicts`: append ` branch=<holder>` to a conflict
  row when `Blocker.Branch != ""` and differs from current tree branch
  (current branch resolved once per command, also best-effort). Field order
  appended at end → existing golden/contract tests stay valid; update
  `help_contract_test.go`/golden only if they pin full rows.
- No store changes: schema, `insertOrRefreshLock`, and scan already carry the
  column. (Still ship as PR — fleet-critical area, CI runs linux `-race`;
  note: linux runner currently offline, macos leg covers `-race` per boot.md.)
- Tests: acceptance-style in `cmd_lock_test.go` (lock inside temp git repo on
  branch X → stored record Branch == X; non-repo → ""); `cmd_check_test.go`
  (blocker with Branch "other" vs tree branch → row carries `branch=other`).

**Task 5 — pin the design calls (the bead's done-signal tests).**
- `TestCheck_BranchNeverWeakensGate` (`cmd_check_test.go`): alice holds
  exclusive on path P with durable live pid and Branch "feature/x"; bob (tree
  on "main") runs `check P` → `blocking=1`, exit 1. Pins path-only keying:
  branch difference must not soften the refusal.
- `TestStampPID_SubagentNoEnv` (Task 2's stub-miss case) + README paragraph
  documenting the subagent env reality (`LOTO_PID`/`LOTO_AGENT_ID` absent in
  background-subagent Bash; ancestor walk is the recovery path) — satisfies
  "test or doc pinning subagent pid propagation."

**Task 6 — docs + follow-up beads.**
- README "Self-healing locks" section: add subagent/ancestor-walk paragraph.
- File (do not implement): cc-plugins bead A — export LOTO_PID/LOTO_AGENT_ID
  into fleet subagent env; cc-plugins bead B (pending Q4) — Bash matcher
  gating git checkout/switch through `loto check`.

**Sequencing:** 1→2→3 (pid lineage), 4→5 (branch), 6 last. Tasks 1-3 and 4-5
are independent; could be two PRs if review load matters, single PR otherwise.

**Done signal (from bead):** `make check` green · gate test for two holders
same path/different branches (Task 5) · subagent pid propagation pinned
(Tasks 2/5 + README) · shipped via PR.

## Out of scope (per bead Do-NOT-touch)

- `internal/identity/registry.go` (loto-aa6 territory) — note Task 2's walk is
  pid-only and does not touch identity resolution.
- `internal/store/tags.go`, `doctor.go` — read liveness, unaffected by input fix.
- cc-plugins /team + loto-check hook code — beads filed, not edited here.
