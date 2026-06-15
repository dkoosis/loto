# Boot
updated: 2026-06-15

→ Queue clear: no open PRs, no ready beads, no locks. Next real work = loto-7sf3 (DEFERRED) — pid liveness=unknown on own exclusive locks + branch-switch gate gap in shared-tree fleets. Genuine design investigation, not triage; un-defer only when dk wants it pursued.

✓ done
- 2 store PRs merged (#183 break-tag-multiholder / #184 lowsev-hardening): multi-holder break+tag fan-out+orphan-tag GC; flock hoist+timeout warn+batch precondition. 6 beads closed (w77f/2nc5/qg0r/9uy5/d4is/13pk). macos -race green, make check clean.
- Deleted stale local team/impl-20260612-1818 (all 6 commits superseded by squash PRs #177–182; `+` cherry markers were patch-id false positives).

‡ traps
- CI linux runner OFFLINE — macos covers `-race`.
- Squash-merge breaks `git cherry` patch-id matching → `+` (unapplied) on already-merged commits. Verify via bead-closed + content-on-main, not cherry alone.
