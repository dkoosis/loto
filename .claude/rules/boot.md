# Boot
updated: 2026-05-29

→ `bd ready` empty + `bd list --status=in_progress`. Real work is the in-flight branches, not the ready queue.

‡ state: bug-audit fixes mid-flight
- 13 beads `in_progress`, 0 ready, 0 open — backlog is NOT drained
- 11 `origin/fix/loto-*` branches pushed, **no PRs open** → work = verify→PR→merge
- 2 beads have NO branch yet: `loto-129` (gh#126), `loto-cq6` (gh#131)
- map: each `fix/loto-X` ↔ bead `loto-X` ↔ one open gh issue

‡ traps
- before closing any bead: confirm fix is on `main` (`git log main | grep loto-X`). gh-issue-closed ≠ code-merged — they drifted hard, reconciled 2026-05-29.
- `git push origin --delete` your fix branch after its PR merges — stale remotes pile up fast.
- `stash@{0}` = old boot.md draft, ignorable.
