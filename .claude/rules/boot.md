# Boot
updated: 2026-06-12

→ Review queue empty. Approve the gated loto-7sf3 plan, then start it: `bd update loto-7sf3 --status open --set-metadata plan_approved=true` (plan on main @374f840 — pid liveness=unknown on own exclusive locks + branch-switch gate gap).

✓ done
- 4 store PRs merged (#177/#178/#181/#182): shouldRestoreOwnerWrite invariant + lock-free downgrade probe. -race green, beads closed.

‡ traps
- CI linux runner OFFLINE — macos covers `-race`.
