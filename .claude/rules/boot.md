# Boot
updated: 2026-05-30 #60

→ pick next from `bd ready` — 4 P3 audit bugs left (loto-j863/ta02/zxjx, + loto-ltof likely WONTFIX). Verify each reproduces against the real binary before fixing (bead/gh state lies about fixed-status here).

✓ done
- #60: no open PRs, no stray branches/worktrees/stashes. `make audit` green. Fixed phantom lint (see trap), `.gitignore`+`.golangci.yml` now exclude `.claude/worktrees/`.
- #53: All 3 P2 audit bugs shipped+merged (#155 h85e, #156 pody, #157 kwlp); beads closed, branches/worktrees cleaned.

‡ traps
- `git commit -am` sweeps daemon-churned docs/NORTH_STAR.md — use explicit `git add`.
- `gh pr merge --delete-branch` breaks under worktree layout — push-delete the branch separately.
- Background impl subagents can resurrect a fix branch/worktree (remote + local) after you've merged+deleted it, off a stale base. Re-check `bd ready` / `git branch -a` after agents report done; the resurrected branch's real content is usually already on main (diff `flagperm.go` etc. = identical) — safe to re-delete.
- Big parallel Bash batches are fragile here: one failure cancels the whole batch and output silently empties. Run git/make steps sequentially; write results to temp files and Read them (stdout buffering ran ~1 turn behind this session).
- Phantom lint findings under `.claude/worktrees/agent-*/`: stale golangci cache + `./...` crawling live sub-agent worktree copies (same module path). Fix: `golangci-lint cache clean`; now excluded in `.golangci.yml`+`.gitignore`. Real source is clean — verify before "fixing".
