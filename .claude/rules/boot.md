# Boot
updated: 2026-05-09 (post-recovery)

→ pick next from `bd ready` — main is clean (b9bb69e), all PRs merged, audit passes.

✓ done
- merged PR #11 + #12 manually after boot.md falsely claimed shipped
- added invariant: ✗ claim "shipped" until `gh pr view N --json state` returns MERGED

‡ traps
- merged worktrees leave stale golangci-lint cache → `golangci-lint cache clean` if `loto-ux3.N/` paths appear in lint output
