# Boot
updated: 2026-05-30 #61

→ only loto-ltof left in `bd ready` (P3, likely WONTFIX — check/status read-locks without op-flock → transient spurious conflict verdict). Decide WONTFIX vs fix. Verify repro against the real binary first (bead/gh state lies about fixed-status here).

✓ done
- #61: 3 P3 audit bugs shipped+merged to main (ta02 chmod Nlink-on-fd, j863 doctor orphan-recovery hint, zxjx permuteWith `--` escape). Beads closed; fix branches/worktrees deleted. Trap: integrated `make check` was red on merge → post-merge lint cleanup (7ac95a2) landed green. Always run full `make check` on main AFTER merging, before pushing.
- #60: no open PRs, no stray branches/worktrees/stashes. `make audit` green. Fixed phantom lint (see trap), `.gitignore`+`.golangci.yml` now exclude `.claude/worktrees/`.
- #53: All 3 P2 audit bugs shipped+merged (#155 h85e, #156 pody, #157 kwlp); beads closed, branches/worktrees cleaned.

‡ traps
- `git commit -am` sweeps daemon-churned docs/NORTH_STAR.md — use explicit `git add`.
- `gh pr merge --delete-branch` breaks under worktree layout — push-delete the branch separately.
- Background impl subagents can resurrect a fix branch/worktree (remote + local) after you've merged+deleted it, off a stale base. Re-check `bd ready` / `git branch -a` after agents report done; the resurrected branch's real content is usually already on main (diff `flagperm.go` etc. = identical) — safe to re-delete.
- Big parallel Bash batches are fragile here: one failure cancels the whole batch and output silently empties. Run git/make steps sequentially; write results to temp files and Read them (stdout buffering ran ~1 turn behind this session).
- Phantom lint findings under `.claude/worktrees/agent-*/`: stale golangci cache + `./...` crawling live sub-agent worktree copies (same module path). Fix: `golangci-lint cache clean`; now excluded in `.golangci.yml`+`.gitignore`. Real source is clean — verify before "fixing".
