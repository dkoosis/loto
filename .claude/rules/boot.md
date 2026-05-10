# Boot
updated: 2026-05-10 (late late)

→ `bd ready` for next P1. Open: #35 (macOS fsnotify regression from atomic-write).

```
gh issue view 35
bd ready
```

✓ shipped this session
- 6 PRs merged: #16 #17 #18 #33 #34 (rebased from closed #32) #36
- gemini fixes applied to #16: trailing-slash in `isPathPrefix`, self-overlap filter, unified JSON schema
- filed #35 — `TestWatchEmitsReservedAndUnreserved` flakes on macOS post-#34
- gitignored `.claude/scheduled_tasks.lock`; dropped `docs/feedback/` (stale review artifacts)

‡ traps
- ✗ ignore #35 — atomic-write needs the temp file out of the watched dir, or kqueue keeps dropping CREATE-on-rename
- ✗ rebase a stale branch onto post-merged main without `git rebase --onto main <old-base> <branch>` — squashed commits cause conflict cascade
