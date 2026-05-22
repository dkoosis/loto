# Boot
updated: 2026-05-22

→ `bd ready` — if empty, ask dk for direction.

✓ done
- ac967ab: identity invariants fixed (stale uuid → error, shape-validation, GC pins session refs, fallback freshness-gated)
- 9a15714: chore sweep (boot trim, linux binaries, loto-tag plan draft)

‡ traps
- parallel session may be mid-`make check` when yours fails — `pgrep -f "make check"` before retry
