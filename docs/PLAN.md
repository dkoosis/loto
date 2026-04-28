# loto plan: current → north star

*Author: Claude. Audience: dk + future Claudes. Companion to NORTH_STAR.md.*

## NS amendments applied (pre-plan)

- **Identity location ambiguity resolved.** NS originally specified two
  conflicting paths for `agents/<handle>.json`. Layout diagram now puts
  identity at `~/.loto/agents/<uuid>.json` (host-global) and drops it
  from `projects/<slug>/`. Rationale: one Claude session touches many
  projects; `LOTO_AGENT_ID` is exported once at SessionStart.
- **`--no-verify` mailbox-logging promise dropped from NS.** A bypass
  means the hook didn't run; nothing else has the staged-paths context.
  `prepare-commit-msg` is also bypassed by `--no-verify`. NS already
  states "trust model = trust the operator" two paragraphs later, so the
  promise was self-contradictory. Now consistent.

## what we have today (2026-04-28)

**Code:** ~500 LOC Go. `loto.go` (library), `cmd/loto/main.go` (CLI), `flock_unix.go`/`flock_other.go`, `loto_test.go`.

**Library surface (`loto` package):**
- `New(baseDir)` → `*LOTO`
- `TryFileLock(agentID, intent, target) (*ActiveLock, error)` — non-blocking, takes shared global + exclusive file
- `TryGlobalLock(agentID, intent) (*ActiveLock, error)` — non-blocking, exclusive global
- `ReadTag(target)` / `ReadGlobalTag()` — descriptive only
- `Break(target)` — **misnamed**: only succeeds when nothing holds the lock. Today's behavior is "reap orphan tag," not "force-release." Real `Break` (forced takeover) does not exist yet.
- `ActiveLock.Unlock()` — idempotent

**CLI surface (`loto` binary):**
- `loto file <target>` / `loto global` — acquire + hold-until-SIGINT
- `loto status [target...]` / `loto break <target>` — see notes above
- Flags: `-base`, `-agent`, `-intent`. Default base is `./.loto` (per-tree).

**Tag schema:** `{agent_id, intent, target, kind, host, pid, branch, cwd, timestamp}`. Atomic write via tmp+rename.

**Invariants honored:** flock is truth (✓), single-host (✓), no daemon (✓), reads free (✓).

**Tests:** `loto_test.go` is single-process. No multi-process / race coverage.

**On disk:** `<base>/global.lock`, `<base>/global.tag`, `<base>/files/<sha256>.lock`, `<base>/files/<sha256>.tag`. Matches NS layout for the inner two tiers; nothing else exists yet.

## known invariant violations / accepted races

So future Claudes don't "fix" them:

1. **TryFileLock open→flock→write-tag gap.** Another process can flock first; current code surfaces this as a typed conflict error, so the gap is benign for correctness (flock is truth). Tag absence is *never* a safety signal.
2. **Unlock removes tag before releasing flock.** Documented in `loto.go:18`. A probe of the tag in this window shows "free" while a `TryLock` would still race; flock remains authoritative.
3. **`gitBranch` shells out per-acquire.** Cold `git branch --show-current` is 10–30ms — half the <50ms acquire budget on NS. Resolution lives in `.21` (cache per-process or stamp the tag once at session start).

## gap analysis: current → north star

Stack-ranked by load-bearingness for the "fresh Claude joins, coordinates in <50ms" acceptance bar.

| # | Gap | NS section | Existing bead |
|---|-----|------------|---------------|
| 1 | **No canonical project-scoped base.** Default `./.loto` per-tree; sibling worktrees can't see each other. | "single canonical base, project-scoped" | loto-7wp.7 (decision) |
| 2 | **No session-persistent identity.** `agent_id` defaults to `pid-N`. Nothing reads `LOTO_AGENT_ID`. | "identity that survives exec" | loto-7wp.9 |
| 3 | **CLI is hold-until-SIGINT toy, not the operating loop.** No `try` (one-shot acquire+release), no `reserve`, no `whoami`, no `inbox`, no `msg`, no `release` by handle, no `install-hook`. JSON output not first-class. Exit codes not stable. | "operating loop" + "Claude-friendly" | loto-7wp.8 → split (see below) |
| 4 | **Holder reports are strings, not JSON.** | "useful holder reports" | loto-7wp.3 |
| 5 | **No mailbox.** | "mailbox piggybacked on the file" | loto-7wp.11 |
| 6 | **No reservations tier.** | "glob reservations as the middle tier" | loto-7wp.15 |
| 7 | **No soft-TTL.** | "soft-TTL on tags" | loto-7wp.14 |
| 8 | **No pre-commit hook.** | "pre-commit hook as the safety net" | loto-7wp.16 |
| 9 | **No `doctor`.** | "loto doctor" | **missing — new bead** |
| 10 | **No cleanup protocol.** | "cleanup is layered" | loto-7wp.13 |
| 11 | **`Break` is misnamed; no real Break exists.** Today's `Break` = reap-orphan. Real Break (forced flock takeover + mailbox notification) is the NS "no silent dispossession" requirement and does not exist. | smell-test invariant + "no silent dispossession" | split — see Phase 1 (rename) + Phase 3 (real Break) |
| 12 | **No dead-PID detection.** | layered cleanup | loto-7wp.5 |
| 13 | **No blocking acquire / context-aware wait.** | operating loop step 3 polling | loto-7wp.4 |
| 14 | **No multi-process race tests, no cross-OS CI.** | testing discipline | loto-7wp.6 + loto-7wp.2 |
| 15 | **No README / NFS warning.** | operator-facing | loto-7wp.1 |
| 16 | **`next` integration story unresolved.** | "composable, not monolithic" | loto-7wp.12 |

## proposed phasing

Four phases. Each phase ends in a usable, shippable state.

### Phase 1 — foundations (decisions + identity + CLI shape + rename)

**Goal:** stable handle, canonical base, CLI shape matches the operating loop, library names are honest.

1. **loto-7wp.7** — decide canonical base. Acceptance must enumerate the four slug-derivation failure modes:
   (a) **no git at all** — fallback to a hash of absolute repo root, surfaced in `whoami`;
   (b) **git but no remote** — same fallback, with a one-time warning;
   (c) **multiple remotes** — prefer `origin`, document the rule;
   (d) **origin URL rewritten mid-project** — slug pinned at first use into a `.loto-project-id` checked-in file (or a stable cache); document that rewrites are a manual reslug.
   Plus: **migration stance** — "no users; `./.loto` is not migrated. Add a one-shot startup warning if `./.loto` is detected in cwd."
2. **loto-7wp.9** — agent identity. `~/.loto/agents/<uuid>.json` (host-global, per amended NS layout). `LOTO_AGENT_ID` env, `loto whoami` reads it. Generation: adjective+noun PascalCase (defer fancy list to .10).
3. **rename `Break` → `Reap` (or `CleanIfOrphan`).** Library + CLI rename pass. Touches `loto.go:222`, `cmd/loto/main.go:56`, callers, tests, docs. *Not* a behavior change. Add a new bead `loto-7wp.18` or amend `.5` to include the rename. Land before `.21` so the new CLI verbs aren't named on top of the wrong concept. The real `Break` (forced takeover) lands in Phase 3 alongside the mailbox.
4. **loto-7wp.3** — typed errors (`ErrHeld{Tag, Kind}`, `ErrIO`, etc.). **Bump priority P2 → P1.** CLI marshals these to NS holder-JSON. Land before `.21` so JSON wrapping has structured errors to format.
5. **loto-7wp.8 → split into three:**
   - **.21 CLI verb rename + JSON wrapper.** `file → try file [--hold]`, `global → try global [--hold]`, all output JSON when stdout not a tty (or `--json`). No new behavior; thin shim over the library. Includes `gitBranch` caching/per-session stamping.
   - **.22 exit-code matrix.** Table-driven test enumerating: `0` success, `1` advisory conflict (held, dead, blocked-by-reservation), `2` usage error, `3` IO/system error. Acceptance is the literal table embedded in the bead and a test that fires every row.
   - **.23 subcommand stubs.** `whoami`, `release [--all-mine|--target|<handle>]`, `status --json`, `inbox`, `msg`, `reserve`, `install-hook`, `doctor` — each returns "not implemented yet" with exit code `2` (or `0` for `whoami`/`status` which Phase 1 fully implements). Establishes the surface area Phase 2/3 fill in.
6. **loto-7wp.1** — README. Documents post-Phase-1 reality, NFS warning, invariants, `--no-verify` operator-trust position.

**Phase 1 exit:** `loto whoami && loto try file foo.go --json` end-to-end with proper JSON on success and on conflict. Library has no misleading names.

### Phase 2 — reliability + cleanup

**Goal:** crashes don't strand state; stale locks self-heal; race coverage in CI.

7. **loto-7wp.5** — dead-PID detection. On acquire, if flock succeeds but a tag exists with a dead PID, lazy-GC the tag. `Reap` learns `--if-dead`.
8. **loto-7wp.13** — cleanup protocol. SessionStart hook runs `loto whoami --ensure`. SessionEnd runs `loto release --all-mine`. `install-hook` writes both.
9. **loto-7wp.4** — blocking + context-aware acquire. `Acquire(ctx, ...)` with polling backoff. CLI `--wait[=duration]`.
10. **loto-7wp.6** — multi-process integration test. Acceptance specifies *two* test shapes verbatim (these belong in the bead body):
    - **(a) contended acquire.** N children `fork+exec` and all call `TryFileLock` on the same target; exactly one exits 0; the rest exit 1 with a holder-JSON payload that names the winner.
    - **(b) crash recovery.** Parent forks child; child acquires; parent SIGKILLs child; parent's next `TryFileLock` on the same target succeeds and the prior tag is GC'd. Verifies dead-PID detection from `.5`.
11. **loto-7wp.2** — GitHub Actions `go test -race` on linux + macos. Includes the `.6` subprocess suite.

**Phase 2 exit:** kill -9 a session, next acquire on the same path succeeds and emits a structured stderr log line (`{"event":"reclaim","prior_holder":"GreenCastle","reason":"dead_pid"}`) noting the reclamation. *Mailbox notification waits for Phase 3.*

### Phase 3 — coordination tiers (advisory layer + mailbox + real Break)

**Goal:** Claudes can stake intent broader than one file, talk to each other, and forced takeovers are observable.

12. **loto-7wp.14** — soft-TTL on tags. `expires_at`, status flagging, GC eligibility.
13. **loto-7wp.11** — mailbox. `<hash>.msgs` JSONL append-only. `loto msg <target> --to <handle> "..."`, `loto inbox --since-acquire`. Acquire-time read built into `try`. **Compaction baked into acceptance:** on read, drop messages older than 30 days (or compact on Nth append, exact rule decided in the bead). Plus: define `loto mbox compact <target>` for manual operation.
14. **real `Break`** (the NS-meaning one) — forced flock takeover on a held lock + appends a system message to the displaced agent's mailbox describing what was broken and why. Implements NS "no silent dispossession." Add as `loto-7wp.19` or as a numbered acceptance line on `.11`. Cannot ship until `.11` exists (mailbox is the notification channel).
15. **loto-7wp.15** — glob reservations. `reservations/<sha256(glob)>.tag`. `loto reserve <glob>`. Acquire surfaces matching reservations as warnings (not blocks). TTL applies.

**Phase 3 exit:** the "5 concurrent Claudes" scenario in NS works end-to-end. Forced break notifies. Reservations warn but don't block.

### Phase 4 — safety net + diagnostics + polish

16. **loto-7wp.16** — pre-commit hook. `loto install-hook` writes `.git/hooks/pre-commit` invoking `loto check-paths --staged`. **`--no-verify` bypass logging is explicitly out of scope** (per amended NS).
17. **loto-7wp.NEW (`doctor`)** — `loto doctor` + `--repair` + `--dry-run`. Acceptance enumerates the five drift classes from NS verbatim, each with a detection method:
    - **stale tags** — tag present, flock unheld, no live PID.
    - **dead-PID holders** — tag's PID does not exist on host.
    - **orphaned `.lock`/`.tag`** — file exists with no matching counterpart, or under a path that no longer exists.
    - **layout drift** — files outside the expected `<base>/{global.*, files/, reservations/}` shape.
    - **soft-stale-but-still-held** — `expires_at` past, but flock currently held (legitimate; flag, do not repair).
18. **loto-7wp.10** — adjective-animal handle list (polish on .9).
19. **loto-7wp.12** — decision on `next` integration. **Deliverable: ADR file at `docs/decisions/0001-next-integration.md`**, linked from the closed bead. Likely outcome: "stay separable; no `loto with-next` until proven need."
20. **loto-7wp.17** — sandbox cross-platform binaries (CI hygiene).

**Phase 4 exit:** NS's six-point acceptance bar is satisfiable end-to-end on a fresh laptop.

## bead metadata table

Rubric: **S** = ≤ $5 (≤ ½ session, ≤ 200 LOC delta), **M** = $5–15 (1–2 sessions, 200–800 LOC), **L** = $15–40 (multi-session, > 800 LOC or cross-cutting). All beads carry `kg_project=loto`.

| Bead | Title (short) | Difficulty | est_cost_usd |
|------|---------------|------------|--------------|
| .7  | canonical base decision (ADR) | S | 4 |
| .9  | agent identity + LOTO_AGENT_ID | M | 10 |
| .18 (new) | rename Break → Reap | S | 3 |
| .3  | typed errors | S | 5 |
| .21 | CLI verb rename + JSON wrapper | M | 8 |
| .22 | exit-code matrix + table test | S | 4 |
| .23 | subcommand stubs | S | 4 |
| .1  | README + NFS warning | S | 4 |
| .5  | dead-PID detection | M | 8 |
| .13 | SessionStart/End cleanup hooks | M | 10 |
| .4  | blocking + context acquire | M | 8 |
| .6  | multi-process integration test (2 shapes) | M | 12 |
| .2  | CI: go test -race on linux+macos | S | 5 |
| .14 | soft-TTL on tags | M | 8 |
| .11 | mailbox + compaction | M | 14 |
| .19 (new) | real Break + mailbox notification | M | 8 |
| .15 | glob reservations | M | 12 |
| .16 | pre-commit hook | M | 10 |
| .20 (new) | doctor + --repair + 5 drift classes | M | 14 |
| .10 | adjective-animal handles | S | 3 |
| .12 | next-integration ADR | S | 4 |
| .17 | sandbox cross-platform binaries | S | 5 |

Total ≈ $175. Estimates anchor decomposition; revise per-bead at execution.

## new beads to add

1. **`loto-7wp.20`** — Phase 4. Acceptance per the five drift classes above.
2. **`loto-7wp.18` rename Break → Reap** — Phase 1. Mechanical rename, no behavior change.
3. **`loto-7wp.19` real Break + mailbox notification** — Phase 3. Depends on `.11`. Implements NS "no silent dispossession."
4. **(.8 split)** — close existing `.8`; open `.21`, `.22`, `.23`.

## bead-edit checklist (for the decomposition pass)

When opening edits against the existing beads, apply *all* of these:

- [ ] Split `.8` into `.21`, `.22`, `.23` (close `.8` as superseded).
- [ ] Open `.18` (Break→Reap rename), `.19` (real Break+mailbox), `.20`.
- [ ] **Bump `.3` priority P2 → P1.**
- [ ] Wire dependencies:
  - `.21 → .9, .7, .3, .18`
  - `.22 → .21`
  - `.23 → .21`
  - `.4, .5 → .3`
  - `.13 → .9, .23`
  - `.6 → .5, .4`
  - `.11 → .23`
  - `.14 → .23`
  - `.15 → .23, .14`
  - `.16 → .15`
  - `.19 → .11`
  - `.20 → .14, .5`
- [ ] Add metadata to every bead: `kg_project=loto`, `difficulty`, `est_cost_usd` (per table above).
- [ ] Embed `.7` failure-mode enumeration (a/b/c/d + migration stance) into bead body.
- [ ] Embed `.6` two test-shape specifications verbatim into bead body.
- [ ] Embed `.22` literal exit-code table (0/1/2/3 with conditions) into bead body.
- [ ] Embed `.11` compaction rule into acceptance.
- [ ] Embed `.20` five drift classes verbatim into acceptance.
- [ ] Embed `.16` "`--no-verify` bypass is out of scope" line.
- [ ] `.12` deliverable is `docs/decisions/0001-next-integration.md`.

## risks / open questions remaining

- **Hook installation across multiple Claude harnesses.** SessionStart/End hook shape differs between Claude Code, Codex, etc. `install-hook` may need per-harness templates. Resolve in `.13` with a "claude-code-only initially; document the shape for ports" position.
- **Test strategy for flock semantics on macos vs linux.** `.2` and `.6` should be designed to surface platform divergence rather than hide it. Concretely: `.6`'s subprocess harness must run on both runners and assert identical conflict JSON shape.

## what this plan deliberately does *not* include

- Multi-host coordination, NFS support, transactional multi-file acquire, permissions/ACLs, daemon, chat features, workflow engine. (NS non-goals.)
- A `loto with-next` wrapper — defer until `.12` says yes.
- Schema migration tooling. (NS smell test #4.)
- `--no-verify` bypass observability — operator's escape hatch by design.

## next step after review

Once dk signs off: run the bead-edit checklist top-to-bottom in one decomposition pass, then start Phase 1 with `.7` (decision first, code second).
