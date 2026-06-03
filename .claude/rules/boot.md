# Boot
updated: 2026-06-02

→ Backlog EMPTY. ∅ ready · ∅ in_progress · ∅ open PRs. Next epic candidate = wt-harness migration (φ docs/wt-harness-migration-brief.md, untracked planning doc — decompose to beads before dispatching).
  ‡ store/race-path → PR, never direct-to-main (linux -race runs CI-only). cli/docs → direct-to-main fine.

≈ cleanup pass 2026-06-02 pm: merged #175 (loto-t8dd), discarded recurring NORTH_STAR stale revert, drained queue. lint `./...` = 0 issues. Repo at rest.

state: epic loto-k5el ✓ COMPLETE. loto-t8dd ✓ (#175). Self-healing advisory file-lease layer (TTL + shared/exclusive) live end-to-end.

✓ done
- #171/#172/#174: loto-k5el epic (.1 TTL self-heal · .2 composite PK+mode+events-CHECK · shared/exclusive+downgrade). Closed.
- #175: loto-t8dd — collapse schemaFullyCurrent into migrationEnsures dry-run gate (one list backs both migrate's apply path and the no-write gate, can't drift). Merged 810b020. Folded 3 Gemini contract fixes (ensureFn returns pending=false after apply); /simplify reviewed clean (F1 explicit-vs-clever tail, F2 narrow-interface, F3 merge-locks-probes — all skipped w/ reasons, altitude already right).
- 86a96a1: feat(cli) case-insensitive repo-path containment on case-folding FS (loto-d3l case variant) — committed direct-to-main by a parallel session mid-cleanup. cli-only, within rules.

‡ traps
- ‡ **CI linux runner OFFLINE.** `gh api repos/dkoosis/loto/actions/runners` → total 0. linux `-race` leg (`runs-on: [self-hosted, Linux, loto]`) can't run — queues indefinitely. macos leg is GitHub-hosted (`macos-latest`) and runs the SAME `go test -race ./...`, so race IS covered there. #175 (store) merged on dk's call: macos CI -race + local -race both green, SQLite-schema change has no linux surface. → bring the self-hosted linux runner back online, or move linux to GitHub-hosted like #173 did for macos.
- NORTH_STAR.md stale revert RECURRED again this session (strips the lock-modes section #174 added) — discarded vs authoritative main. Some parallel-session process keeps regenerating it; `git fetch` + diff-vs-main before trusting worktree doc state. ✗ ever commit it.
- Parallel sessions share ONE working dir here (not separate worktrees). A peer committed cli work (86a96a1) to main mid-cleanup — HEAD/dirty-set move under you. `git fetch` + re-check `git status` before judging tree state; loto-lock before editing shared files.
- wt-harness migration (brief in docs/) = likely next epic: graduate wt-status/wt-gc/wt-land/wt-discard + scripts trixi → loto so bead+code colive. Decompose to beads before dispatching.
