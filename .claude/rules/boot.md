# Boot
updated: 2026-06-15

→ Queue clear — `bd ready` empty, no locks. One PR open: **#188 (draft)** — 6-bead store strip/restore/op-flock fix from `/team backlog`. dk's to review → undraft → merge. Next real work after that = loto-7sf3 (DEFERRED): subagent pid liveness=unknown + branch-switch gate gap. Un-defer only on dk's word.

✓ done
- `/team backlog` drained the 7 bug-audit store beads → PR #188 (v8ch/h760/bvdk/1jxc/3bl0/3qev). make check + store tests green. loto-mzew closed won't-fix (GC read-skew theoretical: 30d cutoff vs ms acquire window).

‡ traps
- squash-merge breaks `git cherry` patch-id → `+` on already-merged commits; verify via bead-closed + content-on-main.
- **`/team` subagents share the primary's loto handle** → the gate can't serialize same-handle agents on the same file; `git commit -am` swept peers' working-tree hunks, `unlock --all` swept peers' locks. Wave-2 commits came out un-sliceable (tree correct, boundaries tangled) → shipped as one integration PR. Before next `/team backlog`: partition wave beads by file with ZERO overlap, or give each subagent a distinct handle.
