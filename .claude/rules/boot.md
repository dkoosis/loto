# Boot
updated: 2026-06-28

## lane: MeldRabbit
branch: main

→ `bd ready` — only ready bead is loto-fs84 (P1): /team subagents share primary loto handle, gate can't serialize same-file edits. Fix the harness; then waves needn't be file-partitioned.

✓ done
- PR triage: merged #194 (loto-45ol, Holder→Owner field rename completing owner= unification; CI green, codex/gemini clean). Bead closed, branch gone. Queue empty.

~ rapport: clipped, decisive — hands the loop and walks; wants flaws named, not worked around.

## lane: RoyalNewt
branch: main

→ `bd show loto-fs84` — B reopened: `agent_id` IS in PreToolUse shell-hook stdin (empirical: distinct per sibling, null at root). Decide fix shape — gate (deny in hook) vs stamp (agent_id→owner_uuid).

✓ done
- Inquiry loto-identity-lock-model: recorded industry scan + empirical CC test; overturned leg-one of the "subagents indistinguishable" dead end. Brief + `industry-agent-id-scan.md` updated.

‡ agent_id-in-shell-hook UNDOCUMENTED — fallback to today's handle; re-test on CC upgrade.

~ design-mode, drove it; nudged the empirical test himself.

## lane: SharpHorse
branch: main

→ `bd show loto-inf4` — only ready bead: P3 AgentUUID typing floor (34n3 stage 1).

✓ done
- Merged #196 (loto-wbkn)+gemini fixes; hook→`loto lock --shared` (loto-25be; codex caught exclusive write-strip→EACCES).
- identity-lock Brief: stamp = detection-only beacon, not preventive; partitioning stays fs84's load-bearing fix.

‡ PreToolUse stamp can't serialize: exclusive self-EACCESes, shared can't deny. Enforcement = deferred check-only gate.

~ drove the fork, decided in one word. Why before the call, loop after.

## lane: EastCobra
branch: main

→ backlog EMPTY (0 open/ready/blocked). Next substantive work = wire `loto lane`/`verify` into the /team fleet harness (9sro spine done; .4 cross-lane integration deferred ❄). No file to run — pick that up or `bd ready` for fresh work.

✓ done
- Closed loto-9sro (parent): spine .1/.2/.3 merged; Codex P2 on #203 fixed inline (store/ctx err → infra exit 3) + tests.
- Merged #204 (loto-dtq5): lane/verify now listed in printHelp as annotated engine verbs. Tree clean on main, queue empty.

‡ fleet impl agent skips golangci — primary's make check eats the lint at wave end (8 goconst on #203).

~ trust-the-loop when fenced.
