# Boot
updated: 2026-05-30 #53

→ pick next from `bd ready` — 4 P3 audit bugs left (loto-j863/ta02/zxjx, + loto-ltof likely WONTFIX). Verify each reproduces against the real binary before fixing (bead/gh state lies about fixed-status here).

✓ done
- All 3 P2 audit bugs shipped+merged (#155 h85e, #156 pody, #157 kwlp); beads closed, branches/worktrees cleaned.

‡ traps
- `git commit -am` sweeps daemon-churned docs/NORTH_STAR.md — use explicit `git add`.
- `gh pr merge --delete-branch` breaks under worktree layout — push-delete the branch separately.
