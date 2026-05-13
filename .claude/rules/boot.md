# Boot
updated: 2026-05-13

→ `bd ready` and ship. main clean (59ef6e3), 0 PRs.

✓ done
- shipped #78 (ErrNoLockAtTarget vs ErrNotOwner) → closed #46
- shipped #77 (CLI behavioral tests for dispatcher + check errors)
- cherry-picked #75 golden help tests; closed #76 (tested deleted Resolve)
- simplify sweep across 4 pkgs (-73 LOC dead exports)

‡ traps
- cwd may be `.claude/`, not repo root — env block can lie; `pwd` to confirm
