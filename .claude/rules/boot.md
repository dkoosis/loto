# Boot
updated: 2026-05-16

→ `bd ready` — only `loto-kez` open (P2 skill-refresh, retired subcommand cleanup).

✓ done
- shipped loto-hz9 (#99 squash-merged) — skill teaches bd-ready triage via `loto check`
- pruned 4 merged stale remote branches; main clean

‡ traps
- `gh pr merge --delete-branch` skips the remote delete if local delete fails (worktree blocks it) — verify with `git ls-remote --heads origin`.
