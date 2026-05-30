# Boot
updated: 2026-05-30 #53

→ merge PR #157 (loto-kwlp PID-reuse fix) once macos CI passes — reviewed, linux green:
`gh pr merge 157 --squash && bd close loto-kwlp`
then push-delete its branch + `git worktree remove .claude/worktrees/agent-a632a9a59fe1fadf0`.

✓ done
- #155 (loto-h85e) + #156 (loto-pody) merged, beads closed; #157 rebased, check+race green.

‡ traps
- `gh pr merge --delete-branch` breaks in this worktree layout — delete branch separately.
- 3 P3 audit beads left (`bd ready`).
