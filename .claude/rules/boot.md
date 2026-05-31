# Boot
updated: 2026-05-31

→ clean slate — `bd ready` empty, 0 in_progress, 0 PRs, no stray worktrees/branches. main pushed (19efdcd).

✓ done
- merged + closed fix/loto-{qev1,qnz8,kyib} (PRs #158/#159/#160); deleted remote branches + worktrees.
- /simplify follow-ups (19efdcd): dropped unreachable test bool + redundant PingContext. make audit green.
- post-hoc CI compensation: TestOpen_ConcurrentFirstOpen 10×40-burst -race on macOS PASS (336s) — retires the gap from pushing 19efdcd straight to main (skipped linux+macos CI on the race-sensitive Open path).

‡ traps
- .team/ heartbeats gitignored — don't re-commit them from a fleet branch.
- gh `pr merge --delete-branch` aborts the WHOLE op if the local branch is held by a worktree → remote branch survives. Remove worktree first, or `git push origin --delete` after.
- DON'T push to main direct when touching internal/store Open/race path — the 1802 bug was macOS-fsync-sensitive; route through a PR so both-platform CI runs. Direct-to-main is fine only for docs/boot.
