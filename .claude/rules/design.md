# Design rules

‡ **stdout audience = Claude.** Every CLI surface except the dashboard (bead `loto-egg`) is consumed by Claude in agent loops. Follow Claude-Optimized Utility Output (nug `32f0ece29b72`) + claudish glyphs (`c75320ff5718`).

- ✓ glyphs over severity words. closed set: ✓ pass/success · ✗ fail (severity, line 1 + per-row) · ℹ neutral data/info row (neither pass nor fail) · ⚠ non-fatal advisory row. banned lookalikes: ✔ (use ✓) · ✘ (use ✗)
- ✓ triage counts on the first body line
- ✓ deterministic sort, same input → byte-identical output
- ✓ `file:line:col` locations, prefer paths relative to cwd
- ✓ inline ```bash fix block under actionable findings
- ✓ explicit empty-status header — silence looks like a crash
- ✗ ANSI color, box-drawing, pluralized prose, repeated field names per row, absolute paths when relative works
- dashboard is the only human-primary surface — different rules apply there
