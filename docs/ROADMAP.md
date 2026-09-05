# roadmap — loto

★ lockout/tagout for files — parallel Claude sessions in one repo coordinate writes instead of clobbering each other

## Epics

One line, one epic — the roadmap orders the epics that make up the project and
names no size between them and the project itself.

1. [done 2026-09-05] git-gate: N agents in one checkout, leased edits land as verified integration commits → loto-ovno (submit → promote → pr live; #296 wired `loto promote`, #300 closed the last child)

Epic 1's four authority levels: shared tree = provisional · candidate = attributed proposal · `refs/loto/integration` = machine-verified · GitHub main = human-accepted. Plan: `~/Projects/dk/Project/loto/plans/git-gate.md` rev 3; decision nug `e4b225b66813` as amended. Supersedes `loto-tzmv`.

Status lives in `.beads/` — `bd show loto-ovno`. This file states order and destination; the bd DAG wins any disagreement.
