# Boot
updated: 2026-05-10 (late late)

→ dk reviews v2 spec one more pass; on approval, `/writing-plans` against it

state: φ docs/superpowers/specs/2026-05-10-loto-v2-design.md

✓ done
- 4th review round integrated: dir-overlap, case-insensitive FS, read_cursors in DB, multi-blocker, corrupt-DB moved-aside

‡ traps
- ✗ project mutex / per-target flock wording — SQLite WAL replaced both; old phrasing keeps creeping back
- ✗ commit `internal/render/llm.go` — pre-session WIP, not ours
- ✗ re-add v1 data migration — explicitly cut
