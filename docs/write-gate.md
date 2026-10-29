# loto's write gate, on one page

**Status:** ratified by dk 2026-10-21 (loto-61g3, merged as #406). This page
is the gate's model; the build below makes the code match it.
**Bound by:** nug `91a8938fa733` (prevent H1, detect H2; nothing reads a
command string) and the conflict-class survey, nug `b2b0a9df507c`.

A change to the gate derives from this table. If it cannot, the table changes
first, and dk ratifies that change.

## The model

| term | definition |
|---|---|
| **actor** | One Claude Code process: `(host, pid, process start time)`, read from its session record. Its subagents, in-process or `isolation: worktree`, are the same actor. `/clear` rotates the session id but not the actor. A shell with no session record is not an actor and gets no protection. |
| **worktree** | The checkout's admin dir within the repo's common git dir: `main` for the primary checkout, `worktrees/<name>` for a linked one. `git worktree move` keeps it. |
| **target** | A path relative to the worktree root, case-folded where the filesystem folds case. A directory target covers every path beneath it. |
| **hold** | The only kind: `(actor, worktree, target, epoch)` plus a lease. Exclusive. `loto lock` and `loto claim` take one; the pre-hook takes one when it admits a gated write (below) of a path its actor does not hold. Beacons and claims are holds; shared mode and `loto downgrade` go. |
| **live** | The hold's actor is not provably dead, and either its lease has not expired or the actor has a call in flight that began before the lease expired. A write to the held target (declared at pre or seen changed at post) renews the lease; nothing else does. |
| **conflict rule** | Actor `a` in worktree `w` may not hold or write target `t` while a live hold of another actor in `w` covers `t` or is covered by it. |

One function decides it: `Decide(actor, worktree, target) → admit | refuse(holder)`.
Every surface calls it: the pre-hook for gated writes, `loto lock`,
`loto claim`, `loto check`, `loto sync`, `loto submit`. Nothing else decides a
write.

**Gated:** Edit, Write, MultiEdit and NotebookEdit, where the path is exact.
**Not gated:** Bash. Its writes fall to H2 detection: before/after digests of
held files report a clobber; nothing refuses it.
**Ref transitions (H1):** the `reference-transaction` hook refuses a HEAD,
stash or branch move in a worktree while another live actor is present in that
worktree. A worktree being born has no actor in it.
**Commits:** not a write decision. The staged-lock gate goes; a commit carries
what the write gate admitted.

## What the model replaces

Seven copies of the write decision diverged (review 2026-10-07):
`gateDecideWith`/`gateLockDenies` (check --gate), `hookRowsDecide` (pre-hook),
`EvalContext.Conflicts` (lock acquire), `computeCheckConflicts` (check),
`syncConflictFor` (sync), `peerLockCovers` (check --held), `gate.Admit`
(submit), plus `handle_deny` in `scripts/gate-file-lock.sh` (deleted by loto-j88z). Each asks "is
this holder me, kin or a peer" from five attributes: session id, subagent
stamp, pid, worktree and branch. The model keeps two: actor and worktree.
`Decide` reads only the holds that cover one target or lie beneath it, in one
worktree, an indexed lookup, against ~100ms per Edit at 200 locks today.

## The kin-or-peer fixes since 2026-09-09

D = derived from the model. X = dropped: the case cannot arise, or its code goes.

| fix | what it settled | verdict |
|---|---|---|
| #349 loto-9zcq | a Write creating its file is admitted holding a lock | D: the rule reads the target, not the file; admission takes the hold |
| #350 loto-0z24 | a subagent's own gate beacon is kin; staged gate exempts it | X: subagents are their actor; beacons and the staged gate go |
| #351 loto-2jgn | one process is one peer after `/clear` | D: actor is the process |
| #354 loto-w0sx | a worktree being born is not a tree move | D: no actor is present in it |
| #355, #358 loto-ay2k, 5q2m | a zero old or new head is a birth | D: as #354 |
| #362 sd-cpbj | own lock outranks a peer's directory claim | D: covers-or-covered refuses the second hold, so the overlap cannot form |
| #371 loto-19bz | `unlock --all` refuses across a branch mismatch | X: branch is not an attribute; release is by actor |
| #372 loto-3eq6 | beacon and lock conflicts scoped to their worktree | D: worktree is in the rule |
| #373 loto-8z87 | one owner's rows in two worktrees coexist | D: worktree is in the hold's key |
| #376 loto-in0v | doctor repairs a moved worktree's stamps | X: the admin dir survives `git worktree move` |
| #377 loto-1h6x | a tree report names its event's worktree | D: worktree is in the hold's key |
| #379 loto-dwu9 | git-status paths fold to the lock key | D: target is case-folded |
| #381 loto-dj7g | sed/perl script words leak into the Bash write-set | X: Bash is not gated |
| #383 loto-y87n | a beacon's rotating session witness cannot override its TTL | X: beacons go; the actor's witness does not rotate |
| #390 loto-6sf4 | a lock sits beside a same-session subagent's beacon | X: subagents are their actor |
| #393 loto-szdx | a call in flight over 6h ends | X: a call with no actor record pins nothing; the 6h bound goes |
| #394 loto-hous | a lock lapses once its holder stops writing that file | D: only a write renews the lease |
| #397 | a root session's write over a peer's lock is blocked | X: the divergent `handle_deny` copy goes |
| #403 | a session releases its rows in every worktree in one unlock | D: release is by actor; the nested-checkout rule goes |

## What the model gives up

- **Siblings in one checkout are not serialized** (loto-xwod). Two subagents
  of one actor writing one file are the actor's own coordination problem;
  H2 detection does not report it, because no other actor spans the change.
- **Bash writes over a peer's hold are reported, not refused.** That is the
  ratified H2 response; the parser that tried otherwise had 12 false refusals
  for 1 prevented incident.

## The build

dk ratified the model; each bead below is open to build.

- loto-njgz: one `Decide` replaces the seven copies.
- loto-j88z: delete the Bash command parser. Built in #410.
- loto-hof2: fold beacons and claims into the hold; retire shared mode and
  `loto downgrade`, since the hold is exclusive.
- loto-e1ku: the actor is the process.
- loto-ojq3: remove the staged-lock gate. loto-6itj is closed with it.
- loto-d0cb survives: a hold stays live while its actor's in-flight call began
  before the lease expired.
