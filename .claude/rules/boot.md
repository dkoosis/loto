# Boot
updated: 2026-05-30 #53

→ merge PR #157 (loto-kwlp PID-reuse fix) once macos CI passes — reviewed, linux green, gemini clean. Then:
`gh pr merge 157 --squash && git push origin --delete fix/loto-kwlp-pid-reuse-start-time && bd close loto-kwlp && git worktree remove .claude/worktrees/agent-a632a9a59fe1fadf0`

✓ done
- #155 (loto-h85e orphan TOCTOU) + #156 (loto-pody unlock --all guard) merged, beads closed.
- #157 (loto-kwlp) rebased on main, probe-sig reconciled, `make check`+race green.

‡ traps
- `gh pr merge --delete-branch` breaks in this worktree layout — push-delete the branch separately.
- 3 P3 audit beads left (`bd ready`): loto-j863/ta02/zxjx; loto-ltof likely WONTFIX.
