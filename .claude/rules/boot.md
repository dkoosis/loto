# Boot
updated: 2026-05-13

state: main clean. No open PRs, no stash, no stale branches or worktrees. `go test ./...` green.

✓ done
- shipped 5 PRs (#64, #65, #66, #69 ex-#67, #70 ex-#68): audit timeouts, loto-vra simplify (-1282 LOC), identity fleet, doctor CC sidecar, store hardening
- ‡ trap learned: deleting a branch on GH auto-closes PRs targeting it. Retarget base BEFORE deleting the base branch, or recreate PRs after.

‡ traps
- loto repo lacks `check-published-files.sh` pre-commit hook → banner-strip silently breaks publish
- gemini-code-assist suggestions noted but not actioned: #67 `filepath.Clean` on sidecar cwd compare; #66 redundant MkdirAll + GC could include tmp files
