# Boot
updated: 2026-05-11

→ `bd show loto-vra.1` — NORTH_STAR republished from KG nug `0b105e61f67f`; review for dk signoff, then close.

state: main clean. PR #63 (loto-qqh.2 multi-target AcquireLocks) open awaiting review.

✓ done
- shipped qqh.2: commit `060f590`, PR #63, `make check` green
- `john-loto-qqh.2/` rebased onto main, all staged work committed

‡ traps
- after #63 merges: `git worktree remove john-loto-qqh.2 && git branch -d loto-qqh.2`
- loto repo lacks `check-published-files.sh` pre-commit hook → banner-strip silently breaks publish
