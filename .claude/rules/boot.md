# Boot
updated: 2026-06-01

→ Queue near-empty. Next ready: loto-t8dd (P3) — store: collapse schemaFullyCurrent into ensure* migration hooks.
  ‡ store/race-path → PR, never direct-to-main (linux -race runs CI-only).

state: epic loto-k5el ✓ COMPLETE. Backlog: 1 ready (loto-t8dd P3). φ docs/wt-harness-migration-brief.md = untracked planning doc (graduate wt-* worktree harness from trixi → loto; dk decision 2026-06-01) — not yet a bead/epic.

✓ done
- #171: loto-k5el.1 TTL self-heal surfacing → merged
- #172: loto-k5el.2 PR A (composite PK + mode col + events-CHECK + legacy round-trip) → merged cdadc511
- #174: loto-k5el.2 PR B (shared/exclusive modes + downgrade + liveness-gated check) → merged 1d6a6cd, CI linux -race green; impl+feat branches deleted. Folded Gemini review (Conflicts incoming/existing) + /simplify (dropped throwaway-struct EffectiveMode idiom). Epic closed.

‡ traps
- loto-k5el epic DONE (.1 ✓ #171, .2 ✓ #172+#174). Self-healing advisory file-lease conflict layer (TTL expiry + shared/exclusive) is live end-to-end.
- During #174 cleanup a stale uncommitted revert of docs/NORTH_STAR.md appeared in the worktree (stripped the lock-modes section) — discarded against merged main (authoritative). Watch for parallel-session clobbers on shared docs; `git fetch` + diff-vs-main before trusting worktree doc state.
- wt-harness migration (brief in docs/) = likely next epic: graduate wt-status/wt-gc/wt-land/wt-discard + supporting scripts trixi → loto so bead+code colive. Decompose to beads before dispatching.
