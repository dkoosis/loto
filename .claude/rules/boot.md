# Boot
updated: 2026-05-10 (afternoon)

→ next: ship lockout primitive (gh#57). v2 has tagout (SQL) but no fs enforcement. Restore flock + chmod-readonly. Read `docs/NORTH_STAR.md` first; v2 plan drifted (marker on line 1).

‡ traps
- Phase 5 (Tasks 21-22 hooks) deferred — depends on gh#57
- `LOTO_AGENT_ID` unset → all Claude sessions share one identity (gh#45)
- gh#46: `loto unlock` misreports missing locks as "not the owner"

✓ done
- filed gh#45/#46/#57; post-mortem marker
- 2 sweep commits + skipped regression test
