# Boot
updated: 2026-05-09 (post-recovery)

→ pick next from `bd ready` — ux3.5, ux3.2, ux3 (epic), loto-0fb.

✓ done — landed on main (31aa397)
- ux3.4 whoami --set-handle (PR #11, cc74e35)
- ux3.6 --on-timeout {block,warn,switch} (PR #12, c2cedf3)
- magloop lint cleanup integrated via ux3.4's 72c76d4

‡ invariants
- ✗ update boot.md to "X shipped" until `gh pr view N --json state` returns MERGED
- ‡ if PR is OPEN, branch is unmerged — do not delete

‡ traps
- merged worktrees leave stale golangci-lint cache → `golangci-lint cache clean` if `loto-ux3.N/` paths appear in lint output
- two parallel feature branches forking from same base → guaranteed conflicts in shared files (doctor.go, loto.go); rebase one onto the other before second merge

φ recovery 2026-05-09 late: prior session committed boot.md claim "ux3.6 shipped" without merging PR #12; PR #11 also open. Subsequent magloop ran on stale main and duplicated 72c76d4's lint fix. Resolved by merging both PRs with conflict resolution, taking ux3.4's canonical helper names (examineZombie, flockOrHeld). PRs auto-closed by GitHub on push. Branches + worktrees deleted.
