# Boot
updated: 2026-05-11

→ `bd show loto-vra.1` — NORTH_STAR republished from KG nug `0b105e61f67f`; review for dk signoff, then close.

state: main clean. `docs/NORTH_STAR.md` carries the auto-publish banner again.

✓ done
- merged PRs #58 #59 #60; pruned 7 branches; closed 8 issues superseded by vra
- restored NORTH_STAR banner + canonical body (May-11 post-cut model); reconciler republishing
- preserved alt "contract-first" draft in `loto-simplify` worktree stash@{0} for later promotion if dk wants

‡ traps
- `john-loto-qqh.2` worktree has 5 uncommitted files staged on schema that now lives in main — rebase before resuming
- loto repo lacks trixi's `check-published-files.sh` pre-commit hook → banner-strip would silently break publish again
