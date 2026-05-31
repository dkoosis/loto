# Boot
updated: 2026-05-31

→ pick next from `bd ready` — loto-t1tq prereqs (loto-aepm P1, loto-l3as P2) lead; 5 P3 bug-audit beads behind. main = 3748ddf, clean, 0 PRs/branches.

✓ done
- merged loto-j1bo (PR #161, squash b285146): TTL-only degrade when no durable LOTO_PID; +Gemini review fixes + /simplify tri-state. make audit clean.

‡ traps
- loto-aepm needs a LIVE session to verify the durable session-pid source.
- internal/store Open/race path → route through a PR (both-platform CI), never direct-to-main.
