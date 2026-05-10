# pass-plan-arch-fit — loto-ux3.1

## verdict
drift-flagged

The plan delivers the bead's acceptance criteria competently and the file/test partition is sound. But it openly proposes a load-bearing semantic addition to the north-star model ("three tiers become four; tag becomes authoritative until TTL") and asks this pass to bless it. That promotion is the thing to push back on — not the implementation shape.

## P0 (blocking on direction)

- **Tier promotion contradicts a north-star invariant, not just the prose.** NS §"the model" lists three tiers. NS §"tags are descriptive, flock is authoritative" is followed by: *"Every protocol decision flows from this."* NS invariant #1: *"flock is truth. Every protocol decision must remain valid if every tag on disk is wrong or missing. (✗ never read a tag and trust it for safety; only for description.)"* The plan's step 3 has `TryFileLock` consult `tag.ExpiresAt` and return `ErrHeld` based on tag content with **no flock held by the holder**. That is exactly "trust a tag for a correctness decision." Calling it a "deliberate semantic addition" doesn't dissolve the invariant — it inverts it. Either the invariant is amended in NS *first* (with an explicit carve-out: "record-tier holds are authoritative-by-tag because no foreground process is available to hold flock") or this plan needs a different mechanism (e.g., a detached holder process tied to a pid file, which the bead rules out — fine, but then NS must move).

- **Frame the addition honestly.** Reservations today are advisory — they *warn*, they don't *block*. The plan has acquire'd tags **block** other agents' `TryFileLock` (return `ErrHeld`, exit 1). That is a categorical change: the new tier is the first record-based tier that *blocks*, not warns. The plan's table says so but the surrounding prose downplays it as "expansion." Recommend the plan-prose be edited to name this directly: *"acquire is the first non-flock blocking tier."* Future Claudes reading the plan need to see that line.

## P1 (architectural concerns)

- **Lazy-reap coverage gap.** Plan §Risks(1) and §Steps(8) note `lazyReapTag` is called from both `TryFileLock` and `TryGlobalLock`. The plan honors TTL there. But stale acquire'd tags also accumulate on paths that are *never tried again* (acquire foo.go, crash, no other agent ever touches foo.go). Today that's caught by `loto doctor --repair`. Confirm `doctor.go` walks all `files/*.tag` and respects ExpiresAt — the plan explicitly says "Files NOT touched: doctor.go." If doctor doesn't honor ExpiresAt today (likely — TTL was reservations-only), record-tier orphans will rot until manual `doctor --repair`. That may be acceptable, but it should be a documented follow-up, not a silent gap.

- **`status` indistinguishability is on the critical path, not the to-do list.** Bead's "excellent vs acceptable" includes: *"Acquire'd locks indistinguishable from `--hold` locks via `loto status`."* The plan defers this to step 8 as a probe with an "if … add a same-pass fix" hedge. If `status` filters by `pidAlive(tag.PID)` (very plausible — ReapIfDead does exactly this at loto.go:321), then post-acquire the holder's process exits, status will show the path as free while another agent's `try` correctly sees ErrHeld. That's the user-visible split-brain the bead specifically forbids. Promote step 8 from probe to required pre-merge check.

- **`lazyReapTag` signature change ripples to `TryGlobalLock` for free.** Plan §Risks(6) acknowledges this. Fine in itself, but combined with the "no global acquire'd persistence is in scope" claim, the door is now open and undocumented — a future bead author reading the code will see "lazyReapTag honors TTL on global tags too" and assume that pathway is supported. Add a code comment at the global call site: *"global tier is process-lifetime only; TTL respect is incidental, not a contract."*

- **`AcquirePath` skips `ConflictingReservations`.** The plan justifies this as hot-path conservation. But the bead's downstream consumer is the hook adapter (loto-ux3.2), where the *whole point* of the pre-write hook is to surface coordination signal to a Claude that's about to clobber a reservation. Skipping the reservation scan there means the hook can never warn "you're about to write a file under another agent's reserved glob." Reconsider: either acquire returns conflicts (small overhead, big signal), or the hook adapter calls reservations explicitly. Don't bake "no" into the lower layer.

## P2 (forward-looking flags)

- **JSON shape is contract.** Plan §Risks(4) says output mirrors `try` shape `{agent_id, intent, target, kind, expires_at}`. Once loto-ux3.2 ships and external hooks consume this, schema changes become breaking. Worth one ADR-style line: "acquire JSON is a stable contract under loto:llm:v1; field additions only."

- **`--wait` exit-3 precedent.** Plan §Risks(5) names this. Good. Suggest adding a one-line note in NS or a new ADR so loto-ux3.6 (`try --on-timeout`) inherits the convention rather than re-litigating.

- **Single-command shape is fine.** `acquire <path> [--ttl] [--wait] [--intent]` is not flag-soup; each flag answers a distinct question. The bead's "no `--mode persist`" admonition is satisfied: there is no mode flag, just defaults. ✓ on this front.

- **Identity-vs-acquire interaction.** Acquire records `tag.AgentID` and `tag.PID`. Identity is host-global (`~/.loto/agents/<uuid>.json`); after a reboot the same UUID reloads and is "the same agent." Acquire'd tags survive process exit but not host reboot in a useful sense — `tag.PID` will refer to a defunct process and TTL is the only reaper. That's correct given NS invariant #2 ("single host"), but worth one test: agent A acquires with TTL=1h, simulate process death (don't release), agent A under same UUID re-acquires within 1h — should extend TTL, not return ErrHeld-by-self. Plan §test 10.re-acquire covers same-process; add cross-process-same-agent.

## ✓ aligns with north-star

- TTL check is genuinely lazy — no daemon, no goroutine, no sweep. Honors NS invariant #3.
- JSON-first emission via existing `EmitJSON` / new `EmitLLMAcquired` honors NS invariant #4.
- Per-path `release <path>` complements `release --all-mine` without breaking it (plan step 6 explicit).
- Idempotent release-of-unheld returns 0, matching NS posture for hook robustness.
- Files-not-touched list is disciplined; scope creep is gated.
- Holder identity rides on errors via `ErrHeld` reuse — no new error-shape proliferation.

## strategic-fit (parent epic loto-ux3)

Yes — this is the right keystone primitive for the epic, and it's well-shaped for loto-ux3.2 (hook adapter) and loto-ux3.6 (`try --on-timeout`). The epic's stated motivating concrete is *"~150 lines of bash + jq to compose `loto try --hold` into cross-event acquire/release."* This bead deletes that bash by giving the hook a primitive that returns immediately and persists. The JSON shape returned by `AcquirePath` will be load-bearing for ux3.2's stdin-JSON adapter, and the exit-3-on-wait-timeout convention will be load-bearing for ux3.6. The risk is not strategic fit; it's that the plan ships before NS is amended to legitimize the new tier, leaving a contradiction on disk between docs and code that future Claudes will read as license to trust tags for other safety decisions.

## north-star recenter

> "Five Claude Code sessions, same repo, different subtrees, each spawning subagents. All editing files. Today they clobber each other or panic on unexpected diffs. loto exists so any Claude can answer one question fast: 'Is it safe for me to edit this path right now, and if not, who's on it?'"

**Improves, conditionally.** The one-question test today returns "yes" if no foreground `--hold` is active, even when another agent has logically claimed the path across two events (e.g., between PreToolUse and PostToolUse). After this bead, the answer correctly returns "no, GreenCastle has it for another 4m via acquire." That's the canonical hook-adapter case and it's a real improvement to fidelity. The risk: if `status` doesn't gain the same TTL-awareness in this same bead (P1 above), the question will return inconsistent answers depending on which command Claude runs — `try` says held, `status` says free. That's worse than today's clean "advisory only" story. Land the status fix in-bead or the improvement is a regression.
