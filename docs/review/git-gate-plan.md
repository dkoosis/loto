<!-- Snapshot. Source of truth: ~/Projects/dk/Project/loto/plans/git-gate.md (kg), rev 3. Copied 2026-08-20 for the review bundle. -->

---
id: d4573ccbaefc
type: project
name: git-gate
tags:
    - project
origin: index.reconcile
created: "2026-08-16T23:41:20-04:00"
updated: "2026-08-17T00:18:50-04:00"
importance: 5
content_modified: "2026-08-17T04:18:47Z"
confidence: 1
cssclasses:
    - project
x.content_hash: b7789cce8502060f90dea4f78a28d0405c1898e5840cf2b9114f5f6247bd0d81
x.quality: 0.56
x.reads: 0
x.views: 0
---
# loto git-gate — end-to-end improvement plan

*W2 plan, pre-decompose, rev 3 (two external reviews folded). Ratifying decision: nug `e4b225b66813` (amended — see Decision log). Source synthesis: nug `7b5d0879d44c`. 2026-08-16/17, session #20.*

**Four authority levels:** shared tree = provisional editing state · candidate = attributed proposed state · `refs/loto/integration` = machine-verified integrated state · GitHub main = human-accepted project state. Correctness lives at the integration boundary — a **loto-owned admission function + atomic `git update-ref --stdin` transactions over in-repo `refs/loto/*`** (no bare repo, no pre-receive; see Decision log). The filesystem is never an authority. Hard write enforcement is a declared non-goal.

```
shared working tree            provisional, mixed, non-authoritative
    ├── leases + ownership epochs
    ├── visible intent · hook feedback (early, not enforcement)
    ├── sticky violation sensor
    └── loto sync              fast-forward unleased paths from integration
            ↓
       lane.Commit             private index · exact write-set · no HEAD mutation
            ↓
   CANDIDATE ENVELOPE          immutable, git-object-bound; expected read from
            ↓                  integration, never the tree
   FAIL-CLOSED ADMISSION       loto function, cheap checks + violation intersect
            ↓
   DURABLE CANDIDATE CLAIM     candidate owns its paths until resolved
            ↓
   PROMOTION (3-phase)         flock → snapshot/claim/construct → unlock
            ↓                  → lane.Verify (no lock) → flock → recheck →
   refs/loto/integration       one atomic ref transaction (update-ref --stdin)
            ↓
   per-bead branch → GitHub PR / human review → main
```

## Outcomes

Observable, externally checkable:

1. **A candidate is rejected, deterministically and attributed, if it touches any unleased path, any path with a stale preimage/epoch, or any path carrying an unresolved violation.** That is the provable claim. Unleased tree mutations are detected best-effort (watcher; no process attribution; a rogue edit inside a holder's own leased file is indistinguishable from the holder's work — residual hole, stated). Scripted demo: rogue `sed -i` on an unleased file → violation recorded, intersecting candidate rejected, integration untouched.
2. **N agents land concurrently from one checkout.** No worktrees, no `git switch`, no agent-side rebase loop. Demo: two agents fork the same base, land disjoint write-sets in either order; both promote with individual attribution. `loto sync` repairs tree↔integration divergence after rejected/abandoned candidates.
3. **In-tree test results are diagnostic only.** Every promotion verifies the prospective integration state in isolation.
4. **A crashed session cannot freeze peers.** Dead leases and dead promotion claims reclaimed via PID + process-start, in seconds. Admitted candidates survive their session (durable claim).
5. **Every rejection is a classified data point** (taxonomy below). PolicyFS reopen trigger = contamination/unauthorized classes only.
6. **Promotion latency is measured and budgeted.** Verify cost in a cut worktree is measured *before* Phase 4 (e6r discipline: evidence before machinery); the plan states p50 promote latency and candidates/hour targets once measured. Batching and unlocked verify must be *earned* by the numbers.
7. **The gate can never become the outage.** `LOTO_GATE=off` bypasses admission with a loud advisory; every bypass is logged as a violation-class event so bypass cannot silently become the norm.

## Non-goals

- **Hard write enforcement** — reopen only on contamination-class rejection evidence.
- **PolicyFS** — suspended; questions parked in Appendix A.
- **A daemon** — promotion in the pusher's process; short flocks serialize (e6r stands).
- **Cross-host coordination.**
- **Replacing GitHub main as human authority** — never "one giant integration PR." Review unit = one PR per bead (working default; disjointness-safe by the lease invariant).
- **True hermetic verify** — `lane.Verify` is *isolated*, not hermetic. Sufficient.
- **Deletion tombstones** — deletion is `result: absent` in the transition record.
- **Batch failure bisection** — interaction failures defeat it. Red-batch rule instead (Correctness model); worst case O(n) verifies, which is where batching's amortization vanishes — accepted, and why Outcome 6 exists.
- **A bare gate repo / receive-pack** — dropped rev 3; buys only object quarantine under this threat model, costs a second object store, hook-install drift, and verify against the wrong repo.

## Constraints & assumptions

- Agents are **cooperative-but-careless**, not adversarial. Nothing here is an OS security boundary; a hostile same-UID process could alter any of it. Accepted.
- macOS primary dev, linux `-race` on CI only → store/identity/gate changes via PR, never direct-to-main.
- Tests stdlib-only; arch linter forbids black-box self-import.
- Reuses shipped `internal/lane` (`Commit`, `Verify`) against `repoTop` — no second repo.
- Session liveness = PID + process-start. flock only for short atomic ops. Same mechanism reclaims stale promotion claims.

## Correctness model

**Candidate envelope (immutable, git-bound).** Binds: candidate ID · proposal commit SHA · agent/session identity · intent/task (bead) identity · exact write-set · per-path transitions · structural ancestry assumptions · lease epoch per path · originating base. Stored in git's object database under `refs/loto/*`. Admission never trusts env vars or mutable sidecar files; a candidate ref without its envelope is rejected. **`expected` preimages are read from `refs/loto/integration` at capture time — never from the working tree** (the tree is provisional; deriving preimages from it reintroduces the freshness dependence rev 2 removed).

**Path-transition CAS — the promotion theorem.** Each changed path records `expected` (blob+mode, or absent) → `result` (blob+mode, or absent) at a lease epoch. *A candidate transition may be applied to the current integration tree iff the candidate held the required lease epoch, every changed path still matches its recorded preimage, and the structural ancestry required to resolve each changed path remains valid. Promotion changes exactly those paths and preserves all other integration state.* Lease epoch = was the agent authorized; CAS = is it still valid now.

**Ancestry assumptions, concretely.** For a created path `a/b/c.go`, the envelope records that `a/` and `a/b/` resolve to trees (not blobs, not absent) in integration. Promotion re-checks exactly those entries — checkable, closes the `pkg/`-moved-under-you hole, never compares whole parent-tree OIDs (false conflicts from sibling changes).

**Ownership epoch semantics.** Renew/heartbeat preserves the epoch. Release+reacquire, transfer, stale-owner reclaim, force-break each increment it. Renewal never self-invalidates healthy work.

**Claim lifecycle.** live session lease → (admission) → durable candidate claim → (promoted / rejected / withdrawn) → released. Claims survive the originating process. Overlapping paths are blocked **at lease acquisition** (fail at the cheap end). No dependency graph between overlapping candidates in v1.

**Admission (loto-owned function, fail-closed, cheap).** Envelope present + proposal SHA matches · actual diff == declared write-set · write-set covered by valid lease epochs · transitions + ancestry structurally valid · commit shape allowed · **write-set intersects no unresolved violation** (correctness check, not UX — this is what stops a leaseholder laundering a rogue edit it didn't notice; activates once the sensor exists, Phase 5). On accept: session lease converts to durable candidate claim; refs written via `update-ref --stdin`.

**`loto sync` — tree↔integration repair.** Divergence arises from rejected/abandoned candidates (tree keeps the junk), out-of-band writes, and interrupted sessions — not from ordinary promotion (promoted blobs originate in the tree). `loto sync` fast-forwards unleased paths to integration content, refuses leased ones, and reports the conflict set per `design.md` output rules.

**Promotion — three phases, verification unlocked.**
1. *Short flock:* snapshot integration SHA · select eligible candidates · claim batch · validate preimages + ancestry · construct deterministic prospective **chain** (integration → apply A → promoted-A → apply B → …) · retain under `refs/loto/promoting/<batch>` · release.
2. *No lock:* `lane.Verify` final prospective state. Admission continues meanwhile.
3. *Short flock:* integration still == snapshot? claim valid? → **one atomic `update-ref --stdin` transaction**: advance integration, retire promoted candidate refs, delete promoting ref. Else release/requeue/rebuild. After a crash, refs alone reconstruct state.

**Red-batch rule.** On verify-red: drop the batch; re-chain the head candidate alone against current integration; verify; promote or reject with attribution; repeat down the queue in deterministic order. Worst case O(n) verifies (see Outcome 6).

**Verify policy is gate-owned.** The invariant verify commands live in loto-owned config outside any candidate tree; repo targets (`make check`) may add checks but never silently redefine the invariant ones.

**Escape hatch.** `LOTO_GATE=off` (or `loto gate off`) bypasses admission for the day the gate itself is broken. Loud advisory on every use; each bypass logged as a violation-class event with timestamp and session.

## Prior decisions

- `e4b225b66813` — gate architecture ratified; hard enforcement non-goal; rejected: worktree-per-agent, chmod, per-agent UID, FUSE-first, long-lived flock liveness, base==HEAD admission. **Amended rev 3:** bare repo + `pre-receive` dropped for in-repo `refs/loto/*` + loto-owned admission (same fail-closed semantics, fewer moving parts; gives up receive quarantine only).
- `7b5d0879d44c` — SMFS/C3/no-mistakes survey.
- `90b671928bbe` — mail retired; loto = locks/claims/tags on file territory.
- Positioning: loto artifacts are named, intent-carrying, expiring; candidate refs + durable claims extend interruption tolerance.
- e6r — no daemon; evidence before machinery. Applied again to verify latency (Outcome 6).

## Decomposition

**Phase 0 — clear the deck + measure** *(independent, ship now)*
- Fix `loto-zssw`, `loto-xwod` (moved here from Outcomes — they're tasks).
- **Measure verify cost**: `hyperfine` on `make check`/`go test ./...` in a cut worktree; record p50; set the latency budget that gates Phase 4 design choices.
- Fold tzmv: close `tzmv.4` (superseded premise); re-home `tzmv.6` (watcher), `tzmv.8`/`.10` (hook-layer bugs), `tzmv.9` (liveness probe) under the new epic.

**Phase 1 — store + envelope foundations**
- Ownership `epoch` column in `internal/store` (schema migration, PR + CI race pass).
- Envelope format + git-object storage; transitions incl. `absent`; concrete ancestry entries.
- Preimage capture from integration ref; verify `lane.Commit` deletion round-trip.

**Phase 2 — admission + namespace**
- `refs/loto/*` namespace; admission function; `update-ref --stdin` transaction helper; escape hatch + bypass logging.
- On accept: lease → durable claim; acquisition-time overlap block.

**Phase 3 — candidate flow**
- `loto submit`: lease check → `lane.Commit` → envelope → admission → `refs/loto/candidates/<id>`.
- Rejection UX per `design.md`. `loto sync` (repair + conflict report).

**Phase 4 — promotion + minimal PR bridge**
- Three-phase promotion; chain construction; atomic ref transaction; stale promotion-claim reclaim; red-batch rule.
- **Minimal integration→branch→PR path: one PR per bead** — the loop closes end-to-end here, not at the end.
- Batching/unlocked-verify complexity gated on Phase 0 latency numbers.

**Phase 5 — instrumentation + watcher**
- Rejection taxonomy: `unauthorized-path · stale-preimage · stale-ancestry · stale-lease-epoch · malformed-candidate · violation-intersect · gate-bypass · verify-red · verify-infrastructure · promotion-race`. `loto gate stats` per class.
- Sticky violations: persist {path, observed_at, fingerprint, lease state, expected owner?, resolution}; resurface at PreToolUse, `loto status`, `loto submit`; admission's violation-intersect check activates here. No process attribution claimed. Watcher optimizes feedback; the gate provides correctness.
- Lease/intent visibility for peers.

## Open questions

1. **Latency budget numbers** — set from Phase 0 measurement; until then batching/unlocked-verify are provisional design, not commitments.
2. **Violation-sensor fidelity** — fsevents vs mtime scan; what fraction of out-of-band writes it actually observes. Bounds how much the violation-intersect check narrows the laundering hole.
3. ~~PR unit~~ — **decided 2026-08-17 (dk delegated)**: one PR per bead. Exception inherited from workflow.md: trivial beads may batch into one PR manually (serial CI runners). Bare-repo drop also confirmed same date.

## Verification criteria

- **Contract tests** (stdlib): naked candidate rejected · diff ≠ write-set rejected · stale epoch rejected · stale preimage rejected · ancestry violation rejected · deletion round-trips · violation-intersect rejected · overlapping lease blocked while claim pending · concurrent submits serialize · promotion-claim reclaim after promoter death · ref transaction atomic (crash between phases → refs consistent, next pusher drains) · bypass logged.
- **CI race pass** (linux) on all store/gate/lane changes.
- **Scripted demo** (`make demo` extension): two agents + one rogue write; Outcomes 1–3 in one run; batching path exercised; `loto sync` repairs a rejected candidate's residue.
- **Metrics live**: `loto gate stats` per-class counts; p50 promote latency reported.

## Appendix A — PolicyFS questions (parked; reopen only on contamination-class evidence)

1. Repo as object/SQLite-backed state only, without breaking Go/Git tooling (plain-directory backing is bypassable same-UID; SQLite backing is why SMFS enforcement holds).
2. macOS localhost NFS under editing/build load, incl. late-ESTALE from NFSv3 write caching.
3. Per-session mount/export as the identity capability.
4. mmap, atomic rename, watchers, gofmt, git FS expectations.
5. Whether reduced wasted work justifies losing the native working directory — answerable from Phase 5 metrics only.

## Progress log
Decomposed 2026-08-17 into epic **loto-ovno** (9 children, deps wired; tzmv superseded). Status, surprises, and decisions now live on the epic's bead comment trail — `bd show loto-ovno` / `bd comments loto-ovno`.

## Surprises & discoveries
- `internal/lane` already implements the hard local-git half — the gate is unusually incremental.
- Rev-1 theorem depended on shared-tree freshness; rev 2 removed it — then rev 2 still let preimage capture read the tree. Rev 3 pins preimages to the integration ref.
- Review 2's "tree still holds pre-A F after promote" is wrong on the happy path (promoted blobs originate in the tree) — but `loto sync` is needed anyway for rejected/abandoned residue and out-of-band writes.

## Decision log
- 2026-08-16 — architecture ratified (`e4b225b66813`); admission/promotion split; no base==HEAD at admission.
- 2026-08-16 — rev-2 calls: transition+ancestry CAS · immutable envelope · durable claims + acquisition-time overlap block · verify outside the flock · atomic ref transaction · chain batching with attribution · epoch semantics · integration ≠ main · verify policy gate-owned · sticky violations · PolicyFS suspended.
- 2026-08-17 — rev-3 calls (to ratify at decompose): **bare repo + pre-receive dropped** (amends `e4b225b66813`) · preimages from integration ref only · violation-intersect is an admission check · `loto sync` component · latency measured before Phase 4 · minimal PR bridge in Phase 4, one PR per bead (working default) · concrete ancestry rule · red-batch rule (no bisection) · `LOTO_GATE=off` escape hatch with logged use.
