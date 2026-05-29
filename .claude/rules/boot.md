# Boot
updated: 2026-05-29 PM

→ next: `loto-4n65` (P3, open) — parent-dir fsync gaps left out of loto-cq6's scope:
- `MkdirAll`-created dirs not fsync'd into parent: `registry.go:269` (sessionDir), `:492` (registryDir).
- `doctor.go` quarantine renames (308/317/326/331) — recovery path, same class.
- pattern to reuse: the `syncDir` helper on `loto-cq6` branch / PR #149.

‡ state: 2026-05-29 PM
- `loto-cq6` (gh#131) SHIPPED — PR #149 open (branch `loto-cq6`), bead closed. Awaiting merge.
  - `syncDir` helper at writeAgent/claimSessionCache/pinSlug. Audit trail: `docs/superpowers/plans/loto-cq6/`.
- 1 open bead (loto-4n65), 0 in_progress. Worktree `loto-cq6/` still present until PR merges.
- all prior audit fixes squash-merged on main (PRs #133–#147). main history clean.

‡ trap learned (the mess that ate a session)
- the bug-audit filed beads AND those fixes were separately squash-merged via PRs — leaving ~11 stale `fix/loto-*` branches that LOOKED unmerged (gh issues open, beads in_progress) but were already on main.
- before merging any branch: `git cherry main origin/fix/loto-X` — `-` = already applied, delete the branch; don't merge (a `--no-ff` merge of an already-merged branch pushes zero-content pollution).
- gh-issue-open ≠ unfixed. bead-in_progress ≠ unfixed. Verify against main commits.
- `stash@{0}` = old boot.md draft, ignorable.
