# Boot
updated: 2026-05-30 #115

→ `bd ready`: only loto-d7sq left (P2 — recoverCorruptSessionCache non-atomic read-judge-unlink can delete a valid winner's cache). Verify repro vs the real binary first — bead/gh lie about fixed-status here.

✓ done
- #115: 3 audit bugs shipped via `/team impl` N=3, merged to main (5caeedc), beads closed, branches+worktrees+remote+PRs cleaned, no locks held.
  - loto-tel0 (P1): `unlock`/`--force` routed through `resolveCLITarget` like lock/check/status/tag — abs paths inside repo no longer hit ErrRepoEscape. Deleted dead `resolveTargets` (its loto-1wl glob TODO is already closed).
  - loto-j325 (P2): 6 direct tests for `acquireRecoveryLock` (timeout/ctx-cancel/contention); no prod change. Red verified by transiently breaking branches.
  - loto-9t0q (P2): `check` filters stale/dead-PID holders via `domain.IsStale` (same trio AcquireLocks uses) so the gate stops demanding `unlock --force` on reclaimable locks. `status` left intact — NORTH_STAR.md:126 wants soft-stale rows flagged there.
  - `/simplify` pass: clean (4 agents converged). Only fix = dropped derivable `p` param from `appendCheckConflictsForTarget`.

‡ traps
- `git commit -am` sweeps daemon-churned docs/NORTH_STAR.md — explicit `git add`.
- gopls fires phantom compiler errors (`undefined: register`, `use of internal package not allowed`) on files in sibling worktrees (`loto-worktrees/`, `.claude/worktrees/`) not in `go.work` — pure IDE noise, in-tree `make check` is truth. Verify before "fixing". (golangci variant: `golangci-lint cache clean`; both excluded in `.golangci.yml`+`.gitignore`.)
- Background impl subagents can resurrect a fix branch/worktree (remote+local) off a stale base after you've merged+deleted. Re-check `git branch -a` after agents report done; resurrected content is usually already on main — safe to re-delete.
- Big parallel Bash batches are fragile: one failure cancels the whole batch, output silently empties. Run git/make sequentially.
- Run full `make check` on main AFTER merging all branches, before pushing — integrated tree catches what per-branch checks miss.
- `gh pr merge --delete-branch` breaks under worktree layout — `git push origin --delete <branch>` separately (worked clean this session).
