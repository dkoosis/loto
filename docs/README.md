# docs/

Index of design + reference docs for loto. Audience: future Claudes and dk.

## top-level

- [DESIGN.md](DESIGN.md) — what loto is for, the design contract, what stays out of scope.
- [../ROADMAP.md](../ROADMAP.md) — the destination and the epics that get there; status lives in `.beads/`.
- [true-bug-audit-2026-04-29.md](true-bug-audit-2026-04-29.md) — point-in-time correctness/concurrency/persistence audit.

## decisions/

ADRs — accepted architectural decisions, append-only.

- [0001-next-integration.md](decisions/0001-next-integration.md) — claim+lock flow with `next`.
- [0002-canonical-base.md](decisions/0002-canonical-base.md) — canonical coordination base directory.

## review/

The bundle for an outside architectural / strategic review — see [MANIFEST.md](review/MANIFEST.md) for what to send and to whom.

- [PROMPT.md](review/PROMPT.md) — the review request; PART A is self-contained.
- [git-gate-plan.md](review/git-gate-plan.md), [pre-tool-use.sh](review/pre-tool-use.sh), [hooks.json](review/hooks.json), [repo-session-hooks.json](review/repo-session-hooks.json) — snapshots of files that live outside this repo, each carrying its source path in a header.

## also here

- [treehouse-prior-art.md](treehouse-prior-art.md) — prior art survey.
- [wt-harness-migration-brief.md](wt-harness-migration-brief.md) — worktree harness migration.

Plans live in the kg (`~/Projects/dk/Project/loto/plans/`), not in this repo. The `superpowers/` plan archive and the `skills/loto.md` snapshot were removed 2026-08-20; both are in git history.
