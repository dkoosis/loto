# Boot
updated: 2026-06-01

→ Merge PR #171 (loto-k5el.1, CI green), then dispatch loto-k5el.2 PR A (migration).
  `gh pr checks 171 && gh pr merge 171`

state: φ docs/superpowers/plans/loto-k5el.{1,2}-*.md — dk-reviewed, impl-ready

✓ done
- #171: loto-k5el.1 SC3 surfacing → PR; .1/.2 plans revised per dk assessment, on main

‡ traps
- loto-k5el gated: .1 merge → .2 PR A → .2 PR B (PR A hand-merges .1's 2 seam files; PR B needs .1's Classify)
- prune worktree impl-loto-k5el.1 post-#171-merge
