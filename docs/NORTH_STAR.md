<!-- auto-published from KG (nug:0b105e61f67f) — edit source nug, not this file -->

# loto north star

*Author: dk. Audience: future Claudes (and dk).*

**One-liner** (`docs/NORTH_STAR_MINI.md`): Lockout/tagout for files — so parallel Claude sessions in one repo coordinate writes instead of clobbering each other.

## what this is for

loto brings [lockout/tagout](https://www.osha.com/blog/lockout-tagout) to files. An agent locks a file while editing it, so no other agent can change it at the same time. The agent tags the file with basic information such as who holds it and what work is being performed (such as a Git issue or bead ID).

loto solves the problem that when multiple Claude Code sessions run in the same repo, they clobber each other or panic on unexpected diffs. (Worktrees just delay the issue until merge.) With loto, a participating agent can instantly see if a file is locked by another team member, and why.

## non-goals

✗ Multi-host coordination (NFS, network shares — flock semantics break).
✗ Long-lived processes across sessions. No daemon.
✗ Enforced consistency. loto assumes a cooperative team and does not prevent a process from changing permissions and directly writing to files.

## end-state acceptance

We reach the north star when a fresh Claude, dropped into any worktree of a project where 4 other Claudes are working, can:

1. Run `loto status` and understand who's on what in <1s.
2. Acquire one or more file locks atomically, and edit safely.
3. Receive a useful blocker report when something is held.
4. Crash, restart, and resume without leaving stale state — including filesystem-mode state.

That's the bar. Everything in the backlog (loto-7wp.*) is a step toward it. Anything else is scope creep.

## see also

- **Design spec** → `docs/DESIGN.md` — model, liveness, lock modes, coordination layers, invariants, tags, smell tests.
- **Elevator pitch** → `docs/NORTH_STAR_MINI.md`.
