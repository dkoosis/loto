# Boot
updated: 2026-07-04

## lane: MeldRabbit
branch: main

→ fs84 CLOSED into inquiry loto-identity-lock-model. Ready queue now: 7af9 (claim, P1) · ebkc · ggue · hnw5 · bo8c. Gate design spec'd → `~/Projects/dk/Project/Inquiries/loto-identity-lock-model/gate-design.md`; dk to decide build.

✓ done
- PR triage: merged #194 (loto-45ol, Holder→Owner field rename completing owner= unification; CI green, codex/gemini clean). Bead closed, branch gone. Queue empty.

~ rapport: clipped, decisive — hands the loop and walks; wants flaws named, not worked around.

## lane: RoyalNewt
branch: main

→ fix shape DECIDED at spec level (2026-07-04): check-only gate + 7af9 path-claims + SubagentStop release → `gate-design.md` in the inquiry folder. dk holds the build call; verify GH #34692 (hooks not firing for some subagent calls) before trusting the gate.

✓ done
- Inquiry loto-identity-lock-model: recorded industry scan + empirical CC test; overturned leg-one of the "subagents indistinguishable" dead end. Brief + `industry-agent-id-scan.md` updated.

‡ agent_id-in-shell-hook now DOCUMENTED (hooks.md common field; SDK v0.2.69; SubagentStart/Stop events carry it too). Fallback stays as hygiene. Build trigger fired: kuv.10 untracked clobber 2026-06-29 (ccp-l1nf).

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

## lane: RoundBuffalo
branch: main

→ ship loto-gate fix — `/ship-plugins` (cc-plugins; ccp-ex66 uncommitted on main).

✓ done
- loto PreToolUse matcher += Bash (mv/cp/rm, git mv/rm): check_path refactor + 12 tests green. Filed ccp-ex66 (fix), ccp-o2w4 (agent_shell bypass, open); ccp-l1nf (within-wave scratch collision) not-loto.

‡ CC hooks fire on Bash tool ONLY — agent_shell/MCP file ops bypass the gate (ccp-o2w4); within-wave still no-op (fs84).

~ corrected my over-confident first verdict — name flaws, verify before asserting.

## lane: RigidLoon
branch: main

→ iterate 4 new beads (7af9 claim · bo8c beacon-collision · hnw5 SendMessage-bridge · ggue `loto help`), then ship ggue → un-defer ccp-dcaba64c.

✓ done
- Filed 4 beads from "smooth ad-hoc multi-agent" recs. #3→ccp-o2w4 (closed); #5 owned by epic ccp-1c309a8e.

‡ rec #5 "fix loto skill trigger" CONTRADICTS settled ccp-1c309a8e (retire skill→tools.md reflex) — ✗ file. ggue = last loto gate for ccp-dcaba64c.

~ design-mode; wants why+caveats, ✗ blind exec — caught #5 conflict.

## lane: StillSwift
branch: main

→ ship ccp-8f273432 (retire loto-coordinate ONLY) after parking its table-setting walkthrough in tools.md/rule — `/ship-plugins` from cc-plugins.

✓ done
- New thinking RETAINS loto skill: reworded epic ccp-1c309a8e + child ccp-8f273432 to coordinate-only retire; dropped child's moot dep on ccp-dcaba64c. ccp-62tc unchanged.

‡ Overturns RigidLoon (boot.md:72) "retire skill settled" — loto STAYS, only coordinate dies.

~ delegated the coordinate call to me; took the rec terse.
