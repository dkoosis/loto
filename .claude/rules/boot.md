# Boot
updated: 2026-04-30

## Design rules

‡ **stdout audience = Claude.** Every CLI surface except the dashboard (loto-egg) is consumed by Claude in agent loops. Output follows Claude-Optimized Utility Output (nug `32f0ece29b72`) + claudish symbols (`c75320ff5718`).
- glyphs ✗ ⚠ ℹ ✔ over severity words; counts on first line; `file:line:col` locations; deterministic sort; drop passes; no ANSI / box-drawing.
- ✗ pluralized prose, ✗ repeated field names per row, ✗ absolute paths when relative works.
- dashboard is the only human-primary surface — different rules apply there.

## Queue

→ pick from `bd ready`. Next: loto-egg (dashboard) is the main open work.

✓ done
- /sweep craft across 3 pkgs: 8cf64c0, 038da07, 40d4f56
- loto-dit closed — all 4 nolints removed via examineLockPair / loadTag / reapTagIfMine extraction
