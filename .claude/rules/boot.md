# Boot
updated: 2026-07-20

## lane: FullFalcon
branch: main

→ `bd ready` — queue EMPTY. Both remaining beads deferred to 2027-01-20.

✓ done
- Cleared the backlog: 6 PRs merged (#217–#222), 9 beads closed. Fixed 2 reviewer findings inline (CodeRabbit bind-var, Codex two-tx atomicity).
- Froze e6r + hnw5 — findings in bead bodies, `human`-labeled, deferred 6mo.

‡ e6r: sqlite OPEN is 3ms, NOT the ~1s CLI cost — daemon needs a pprof on an idle box first. hnw5: bridge infeasible, gap covered by loto tag + native SendMessage.

~ triage-mode: names flaws, merges, walks; froze the judgment-call spikes rather than have me pick architecture.
