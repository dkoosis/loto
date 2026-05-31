# Boot
updated: 2026-05-31

→ pick next from `bd ready` — loto-l3as (P2 SessionEnd hook), loto-0gsu (P2 bug), loto-u7b7 (P2 AGENT_ID hook broken); 4 P3 bug-audit beads behind. main = b6753c3, clean.

✓ done
- closed loto-aepm + loto-t1tq (both P1, direct-to-main b6753c3): SessionStart hook PPID-walks to durable `claude` ancestor → exports LOTO_PID. Verified live + tests green; HEAD installed to ~/go/bin/loto.

‡ traps
- NEXT-SESSION CHECK (confirms aepm hook auto-populates): `echo $LOTO_PID; kill -0 $LOTO_PID` — both must succeed. If unset, hook didn't fire / PPID chain changed.
- loto-u7b7: pre-existing LOTO_AGENT_ID hook is broken (`whoami --json` emits human text, python swallows). Defeats degraded-pid warning too.
- internal/store Open/race path → route through a PR (both-platform CI), never direct-to-main.
