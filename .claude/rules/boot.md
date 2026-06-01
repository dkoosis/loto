# Boot
updated: 2026-06-01

→ Dispatch loto-k5el.2 PR B (feature) per docs/superpowers/plans/loto-k5el.2-shared-exclusive.md.
  Tasks 2-5, 5.5, 7-9: domain Mode/Conflicts, write-bit, DowngradeLock, CLI --shared/downgrade, liveness-gated check.
  ‡ store/race-path → PR, never direct-to-main (linux -race runs CI-only).

state: φ docs/superpowers/plans/loto-k5el.2-shared-exclusive.md — Tasks 2-5,5.5,7-9 = PR B, impl-ready

✓ done
- #171: loto-k5el.1 SC3 surfacing → merged; worktree pruned
- #172: loto-k5el.2 PR A (composite PK + mode col + events-CHECK + legacy round-trip) → merged cdadc511, CI linux -race green; branch deleted

‡ traps
- loto-k5el gated: .1 merged ✓ → .2 PR A merged ✓ → .2 PR B (now unblocked)
- PR B hand-merges .1's 2 seams: cmd_status.go::printStatusLocks + locks_acquire.go::reclaimStaleAndCollectBlockers. PR B consumes .1's Classify for liveness-gated check --staged.
- PR A folded two deliberate deviations (in #172 body + commit): legacy round-trip is raw-SQL not domain-level; insertOrRefreshLock ON CONFLICT composite-key fix pulled forward from T3 (PK change breaks the upsert).
