# Boot
updated: 2026-05-14

→ check PR #82 status; merge if approved (`gh pr merge 82 --squash --delete-branch`), then `bd ready`.

✓ done
- shipped epic loto-253 (north-star drift cleanup, 7 children) + loto-81n (P1 session-uuid scoping) as PR #82
- closed stale loto-ddp, loto-flg (post-cut files gone)

‡ traps
- editing files in /Users/vcto/Projects/loto/ root fails inside a worktree session — Edit tool routes to worktree path; use `git worktree list` to confirm cwd.
