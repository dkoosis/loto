# roadmap — loto

★ lockout/tagout for files — parallel Claude sessions in one repo coordinate writes instead of clobbering each other

## Epics

- **loto-ovno — git-gate: N agents, one checkout, leased edits land as verified integration commits** *(in flight)*
  Four authority levels: shared tree = provisional · candidate = attributed proposal · `refs/loto/integration` = machine-verified · GitHub main = human-accepted. Plan: `~/Projects/dk/Project/loto/plans/git-gate.md` rev 3; decision nug `e4b225b66813` as amended. Supersedes `loto-tzmv`.

*Status lives in `.beads/` — `bd show loto-ovno`. This file holds direction only.*
