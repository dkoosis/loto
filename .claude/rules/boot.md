# Boot
updated: 2026-05-10 (afternoon)

‡ **PRIORITY FLIP**: v2 ships tagout without lockout. `AcquireLock` writes a SQL row — no fs-level enforcement. The padlock is missing. Hooks/skills are the trained-worker layer; they presuppose a primitive that doesn't exist. → see gh#57.

→ next: restore lockout primitive — flock at AcquireLock (port from v1) + chmod-readonly fallback for tools that ignore flock. Then gh#45 (identity collision). Hooks/install-hook (Tasks 21-22) **deferred** until primitive lands.

✓ done
- v2 plan Tasks 0–20, 23–25: domain, store, identity, CLI, acceptance + concurrent + crash tests
- v1 deleted; main.go → v2 dispatcher; smoke green

⚠ deferred (do not work these until primitive lands)
- v2 plan Tasks 21-22 (hooks + install-hook)
- loto skill polish, install-hook ergonomics, /sweep coordination

‡ traps
- `LOTO_AGENT_ID=""` (set+empty) forces new identity; unset → most-recent on host (→ gh#45: collapses sessions)
- `loto unlock` returns "not the lock owner" for missing locks (→ gh#46)
- `make check` ~58 lint (goconst glyphs, rangeValCopy, G115). Batch via /polish.
