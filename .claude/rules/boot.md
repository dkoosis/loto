# Boot
updated: 2026-05-31

→ clean slate — PR #161 merged+closed, `bd ready` = 6 (1 hook bead + 5 P3 bug-audit beads), 0 in_progress, 0 open PRs, no stray branches/worktrees/stashes. main = b285146.
→ next: `loto-t1tq` (open, P1) is 2/3 done — needs `loto-aepm` (durable LOTO_PID hook export) + `loto-l3as` (SessionEnd release). aepm needs a LIVE session to verify the session-pid source.

✓ done
- shipped + merged loto-j1bo (PR #161, squash b285146): PID-0 sentinel + IsStale PID>0 guard → lock degrades to TTL-only without LOTO_PID, kills always-on silent-clobber. TDD, +race, both-platform CI green.
- PR review pass (Gemini): stampPID → (pid, pidSource) tri-state so the degrade warning names unset-vs-invalid without re-reading env; IsStale nil-probe guard (zero-value EvalContext → TTL, no panic). /simplify converged on the same tri-state. make audit clean.

‡ traps
- macOS CI queue is variable (3min–35min+); pending ≠ failing — wait it out, don't admin-merge.
- loto-aepm needs a LIVE session to verify the durable session-pid source (CC session pid vs PPID-walk).
- DON'T push to main direct when touching internal/store Open/race path — route through a PR so both-platform CI runs. Direct-to-main is fine only for docs/boot.
