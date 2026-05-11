# Boot
updated: 2026-05-10 (Sun morning)

→ ship lockout primitive (gh#57). v2 has tagout, no fs enforcement. Restore flock + chmod-readonly per `docs/NORTH_STAR.md` tiers 3-4. Re-read it first.

‡ traps
- Phase 5 hooks blocked on gh#57 — hook alone is post-it, not enforcement
- LOTO_AGENT_ID unset → sessions share identity (gh#45)
- gh#46: `loto unlock` mis-reports missing as "not owner"

✓ done
- v2 bug audit: 10 issues #47-#56 + 10 beads (loto-erj/dmk/0o6/200/l6o/7c0/16t/cwg/hwj/ri4)
- fix order in nug 4ac644c95710; after #57, start #50/loto-200 (identity atomic write)
