# Boot
updated: 2026-05-14

→ `bd ready` — qqh epic shipped, pick next.

✓ done
- shipped epic loto-qqh (lockout primitive, gh#57): chmod strip-write enforcement, multi-target atomic AcquireLocks, op-flock serialization, doctor orphan-mode scan + --restore-orphan-mode, render package
- closed gh#46 (folded into qqh — ReleaseLocks distinguishes no-lock vs not-owner)

‡ traps
- editing files in /Users/vcto/Projects/loto/ root fails inside a worktree session — Edit tool routes to worktree path; use `git worktree list` to confirm cwd.
