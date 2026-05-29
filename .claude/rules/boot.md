# Boot
updated: 2026-05-29

→ `bd list --status=in_progress`. 1 bead genuinely unfixed:
- `loto-cq6` (gh#131) — atomic-rename publish doesn't fsync parent dir. See `paths.go:91` (`_ = os.Rename(tmpName, pinFile)`, no dir sync) + doctor.go rename sites. No branch, real work.

‡ state: branch backlog fully drained 2026-05-29
- 0 ready, 0 open, 2 in_progress. No `fix/loto-*` branches remain; `origin/main` is the only branch.
- all prior audit fixes are squash-merged on main (PRs #133–#147). main history is clean.

‡ trap learned (the mess that ate a session)
- the bug-audit filed beads AND those fixes were separately squash-merged via PRs — leaving ~11 stale `fix/loto-*` branches that LOOKED unmerged (gh issues open, beads in_progress) but were already on main.
- before merging any branch: `git cherry main origin/fix/loto-X` — `-` = already applied, delete the branch; don't merge (a `--no-ff` merge of an already-merged branch pushes zero-content pollution).
- gh-issue-open ≠ unfixed. bead-in_progress ≠ unfixed. Verify against main commits.
- `stash@{0}` = old boot.md draft, ignorable.
