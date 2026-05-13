# Boot
updated: 2026-05-12

→ `bd show loto-vra.1` — NORTH_STAR republished from KG nug `0b105e61f67f`; review for dk signoff, then close.

state: main clean. No open PRs, no stash, no stale branches or worktrees.

✓ done
- #63 (loto-qqh.2 AcquireLocks) merged + 2 Gemini bug fixes (dedup blockers, RolledBack flag)
- lint fixes: rangeValCopy, funlen, doctor test missing -t
- pushed main 02dce86; remote branch loto-qqh.2 deleted; worktree removed

‡ traps
- loto repo lacks `check-published-files.sh` pre-commit hook → banner-strip silently breaks publish
