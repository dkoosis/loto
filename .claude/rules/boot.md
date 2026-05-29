# Boot
updated: 2026-05-29 #7

→ next: nothing claimed. `bd ready` = empty, no open PRs, no in_progress beads. Backlog drained.
- `loto-4n65` SHIPPED + MERGED — PR #151 squash-merged to main (583a688). Worktree + local/remote branch deleted. gemini review (top-down fsync order via slices.Backward + dropped TOCTOU stat in fillCorruptStaging) addressed in 0c37337 before merge. `make audit` green both CI platforms.
- `loto-cq6` (gh#131) merged via #149 earlier.

state: φ docs/superpowers/plans/loto-4n65/{plan.md,pass-1-review.md} (Pass C records the gemini fixes)
- `.quality/ledger.db*` — local lintbrush ledger, untracked, not mine; left in place (NOT gitignored — don't commit it).

‡ CI runs on self-hosted runners (`trixi-loto` Linux, `mac-loto` macOS) — GH-hosted billing block bypassed.
‡ trap: gh-issue-open ≠ unfixed, bead-in_progress ≠ unfixed. Before merging any branch: `git cherry main origin/<branch>` — `-` = already on main, delete don't merge.
