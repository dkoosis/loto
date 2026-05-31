# Boot
updated: 2026-05-31

→ `gh pr checks 161`; when macOS green, merge PR #161 and `bd close loto-j1bo`. macOS was queued ~25min at wrap.

✓ done
- shipped loto-j1bo: PID-0 sentinel + IsStale PID>0 guard → lock degrades to TTL-only without LOTO_PID, kills always-on silent-clobber. TDD, +race green.
- reframed loto-t1tq (lease-liveness); split out loto-aepm + loto-l3as.

‡ traps
- macOS CI queues long (~25min+); pending ≠ failing.
- loto-aepm needs a LIVE session to verify the durable session-pid source.
