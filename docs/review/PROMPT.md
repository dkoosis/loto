<!-- Snapshot. Source of truth: ~/Projects/dk/Project/loto/architecture-review-prompt.md (kg). Copied 2026-08-20 for the review bundle. -->

---
id: c44df3fd6a61
type: project
name: architecture-review-prompt
tags:
    - project
origin: index.reconcile
created: "2026-08-20T15:24:47-04:00"
updated: "2026-08-20T15:24:47-04:00"
content_modified: "2026-08-20T19:26:54Z"
cssclasses:
    - project
x.content_hash: 28d42894272f252be6654ca5dbe064ebaee1bc963c49efc1a6b44e18cced73b5
---
# loto — architectural / strategic review prompt

*Paste PART A + PART B into any LLM. For humans, send PART A + the short form at the bottom. Reviewer answers are comparable across models because the output shape is fixed.*

---

## PART A — the brief (self-contained; assume the reviewer knows nothing)

**What loto is.** A Go CLI that coordinates file edits between concurrent AI coding sessions in one git repo. An agent locks paths with a stated intent before editing; another agent that tries the same path is blocked and shown who holds it and why.

```
loto lock internal/store/store.go -t "loto-abc: refactor query path"
loto check <paths>     loto status     loto unlock <paths> -t "done"
```

**The problem it claims to solve.** Several Claude Code sessions, subagents, and shells run in the same repository at once. Two of them edit the same file; one set of changes is silently lost, or an agent panics on a diff it did not write. Git worktrees do not fix this — they defer the collision to merge time, and agents are bad at merges.

**Premise 1 — pessimistic locking, deliberately.** loto grafts RCS/SCCS-style pessimistic locking (`co -l`: check out, everyone else is locked out) onto git's optimistic model. Pessimistic locking lost that historical argument to distributed human teams: week-long holds, absent holders, no central authority. The claim is that multi-agent solo-laptop development inverts every one of those parameters — one human authority, holds measured in minutes, a single host, and provable liveness plus a TTL that kills the vacationing-holder problem outright. git remains the history and distribution layer; loto is a pessimistic edit layer above it.

**Premise 2 — interruption tolerance, not just conflict prevention.** Git's orphan artifacts (stashes, branches, worktrees) are anonymous, intentless, and immortal: after any interruption they become forensic debt. A loto lock is named, carries intent, and expires. The claim is that surviving interruption is at least as much of the value as preventing clobbers.

**How it is built.**
- Go CLI, no daemon, single host by design. Multi-host is an explicit non-goal.
- State in SQLite under `$XDG_STATE_HOME/loto/projects/<slug>/loto.db`, project scoped by git-remote-derived slug so sibling worktrees of one repo coordinate transparently. A short-lived flock serializes DB operations only — it is never held across user work.
- Agent identity is the Claude Code session id (`CLAUDE_CODE_SESSION_ID`), host-global and never stored; `loto whoami` records the session's liveness witnesses at `~/.loto/session/<sid>.json` once at session start. State is project-scoped.
- **Enforcement is cooperative plus one harness hook.** loto does not change file permissions. Earlier versions did `chmod` strip-write on acquire; that was retired because it locked out the holder too. Today a Claude Code `PreToolUse` hook refuses a *peer's* write over a held path. Anything that ignores the hook can still write.
- **Liveness-primary reclaim.** Each lock stamps the owning session PID plus process start time (defeats PID reuse). A holder whose session is provably dead is reclaimed in-line on the next `lock` or `check`. A 30-minute TTL is the backstop for cases liveness cannot cover (bare shells, cron, host reboot). A provably-live session is never TTL-reaped mid-edit.
- Output is machine-first: fixed glyphs, one record per line, deterministic order, because stdout's audience is an LLM.
- ~2,500 symbols across 9 packages. Single maintainer, working with agents.

**What was tried and dropped.** loto once carried agent-to-agent mail (inbox/send/poll). It was removed when the harness shipped native cross-session messaging. The one surviving case — leave word for an agent not yet running — became a tag pinned to file territory instead of a message to an address.

**What is being built now (the active epic, "git-gate").** N agents in one shared checkout, with leased edits landing as verified integration commits, across four authority levels: the shared working tree is *provisional*; an agent's committed candidate is an *attributed proposal*; `refs/loto/integration` is *machine-verified*; GitHub `main` is *human-accepted*. Branch refs are built by pure git plumbing — no checkout, no HEAD move — so parallel lanes can commit disjoint write-sets out of one working tree.

**The decision surface — choices made, and what was passed over.** Judge these; the thing I am most afraid of is an option never on the table.

| Question | Chosen | Passed over |
|---|---|---|
| Where does coordination truth live? | SQLite rows, project-scoped, plus a short op-flock | Lock files in the tree · flock held foreground for the edit's duration · a daemon holding state in memory · git refs as the lock table |
| What stops a bad write? | A harness `PreToolUse` hook refusing a peer's write; otherwise cooperation | `chmod` (tried, retired — it locked out the holder too) · a FUSE/overlay filesystem that enforces at the syscall · a wrapper that owns every edit (`loto with <cmd>`, built, deferred) · nothing at all |
| What is the unit of a lock? | A canonicalized path, exclusive or shared, held by an agent | A symbol or function range · a package/directory lease · a whole-tree turn-taking token · a declared write-set checked at commit rather than held |
| How does a dead holder get cleaned up? | Session PID plus process start time, reclaimed in-line; 30-minute TTL as backstop | A heartbeat · a daemon reaper · human-only `loto doctor` · no reclaim, holds are permanent until released |
| How do agents share a repo? | One shared working tree, coordinated writes; git-gate adds per-lane commits by plumbing | One worktree per agent plus a merge queue · one clone per agent · a copy-on-write snapshot per agent, diffed back |
| When is a conflict detected? | Before the edit, at lock time (pessimistic) | After the edit, at commit or merge (optimistic) · continuously, by a watcher · never — detect the clobber and re-run the loser |
| Who arbitrates? | Nobody; peers negotiate through rows and tags | A central orchestrator that assigns work so files never overlap · the human, prompted on every collision · a scheduler that plans disjoint write-sets up front |
| Scope of the system? | One host, no daemon, no network | Multi-host · a shared service · a hosted control plane |

**Context that matters.** This is one person's tooling, built to make his own multi-agent workflow survivable. It is not a funded product, has no users besides its author, and competes for his time with the work it is meant to accelerate. The harness vendor (Anthropic) ships new coordination primitives every few weeks and has already absorbed one loto feature.

---

## PART B — the review request

You are reviewing loto's **premise and strategy**, not its code style. Be adversarial. I want the review a skeptical senior architect gives when they are not worried about my feelings and are not being paid to be encouraging.

Ground rules:
- No praise, no preamble, no summary of the brief back to me.
- Lead with your verdict. Every claim gets a reason; contestable claims get a mechanism.
- Attack the framing if the framing is wrong. "You are solving the wrong problem" is a legitimate answer and I would rather hear it now.
- Where you are guessing, say so. Do not invent facts about the code.
- Under 1,800 words total; spend most of them on sections 5-7. Prose over bullets where the reasoning matters.
- Before you evaluate my design, generate your own. I would rather read three implementations I have not thought of than a careful grade on the one I already built.

Answer these, in this order, with these headings:

**1. Verdict.** One line: *continue* / *narrow* / *pivot* / *kill*. Then one sentence of why.

**2. The premise.** Is pessimistic locking over git the right shape for multi-agent coding, or a category error? Address the inversion argument directly — do minute-long holds, one host, and one human authority actually rehabilitate a model that failed for human teams, or do agents reintroduce the same failure in a new form?

**3. The strongest case against.** Build the best argument that loto should not exist — that the problem is transient, better solved a layer up or down, or not real at the scale one person operates at. Make it the argument I would find hardest to dismiss.

**4. The steelman.** Now the strongest case *for*, even if you voted kill. What is the durable thing here that would still matter if the harness shipped native locking next month?

**5. Design it yourself, first.** Ignore my implementation. You have the problem statement from PART A and nothing else. Sketch **three** materially different implementations you would consider — different in mechanism, not in polish. At least one must not use locks at all. For each, give: the core mechanism in two or three sentences, what it makes easy, what it makes impossible, and the failure mode that would kill it. Then say which of the three you would build, and why.

**6. What is missing from the decision surface.** Look at the table in PART A. Name the options that should be in a row and are not — mechanisms, primitives, or framings nobody put on the table. This is the section I most want to be surprised by. For each: why it plausibly belongs, and what it would cost to find out. Prior art counts — say plainly if some other field solved this thirty years ago and I am rebuilding it badly (distributed databases, filesystems, revision control, build systems, real-time collaborative editing, operational transforms, transactional memory, air-traffic control, whatever actually applies).

**7. Grade the choices I did make.** Now, for each row of the decision surface: right, wrong, or unclear, and the reason in one or two sentences. Say which single row you would change first and what it would cost me.

**8. Alternatives it must beat.** For each of these, say whether loto is better, worse, or unclear, and why — one short paragraph each:
   a. One worktree per agent, plus a merge queue.
   b. A single orchestrator process that serializes all writes and never lets two agents touch one file.
   c. No coordination: let agents collide, detect the clobber after the fact, and re-run the loser.
   d. Waiting for the harness vendor to ship this natively.

**9. The riskiest structural choices.** Name the three most dangerous, worst first. For each: the failure mode in concrete terms, and what you would do instead. Candidates worth your attention (not a limit): cooperative enforcement resting on one harness hook; SQLite rows as coordination truth when the design's own principle is "truth, not tags"; liveness-by-PID plus a 30-minute TTL; project identity derived from a git remote; single-host as a permanent non-goal.

**10. Scope.** The git-gate epic (four authority levels, candidate commits, a machine-verified integration ref) is materially more ambitious than file locking. Is it the same product or a second one wearing the first one's name? If it is a second product, say what that means for both.

**11. The 80% version.** If I had to cut this to the smallest artifact that captures most of the value, what is it? Be specific — name what survives and what dies.

**12. Falsifiable predictions.** Three things that will be observably true within six months if the premise is right, and three if it is wrong. They must be checkable without my judgment.

**13. What would change your mind,** and the single question you would ask me before committing to your verdict.

---

## Short form (for humans — send PART A plus this)

Four questions, answer any or all, however briefly:

1. Is the premise sound — pessimistic file locking bolted onto git, for AI agents rather than people? Where does it break?
2. What would you build instead to solve the same problem, and why is it better?
3. Enforcement is cooperative: one harness hook stops a peer's write, and anything ignoring the hook writes freely. Is that a fatal compromise or the right amount of engineering?
4. Forget how I built it. Given the problem, what mechanism would you reach for first — and is there prior art I am obviously reinventing?
5. If you had to kill this project, what is the argument you would use?

---

## Notes for dk (not part of the prompt)

- Reviewers **with** repo access: attach `docs/DESIGN.md` (the contract — invariants, non-goals, smell tests), `ROADMAP.md`, and `~/Projects/dk/Project/loto/plans/git-gate.md`. Add one line: "DESIGN.md is the contract; ROADMAP.md is the direction; disagree with either."
- Keep PART A byte-identical across reviewers or the answers stop being comparable.
- Sections 5 and 6 are the point of this revision: divergence before critique. A reviewer who skips straight to grading my design has answered a cheaper question than the one asked.
- Section 12 (falsifiable predictions) earns its keep at the six-month mark. Diff the models on it.
- Read section 5 answers across reviewers *before* reading any section 7. Their independent designs converging on something I did not build is the strongest signal available here.
- Expect LLMs to converge on "narrow" and "the harness will absorb it." Weight the humans on sections 3, 5, and 6.
