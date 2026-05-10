# Boot
updated: 2026-05-10 (overnight)

→ ship Tasks 21+22 (hooks + install-hook); v1 wiped, no fallback. Start: `rg -n "Task 21" docs/superpowers/plans/2026-05-10-loto-v2.md`

✓ done
- v2 plan Tasks 0–20, 23–25: domain, store, identity, CLI, acceptance + concurrent + crash tests
- v1 deleted; main.go → v2 dispatcher; smoke green

‡ traps
- `LOTO_AGENT_ID=""` (set+empty) forces new identity; unset → most-recent on host
- `make check` ~58 lint (goconst glyphs, rangeValCopy, G115). Batch via /polish.
