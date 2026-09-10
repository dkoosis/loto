# Boot
updated: 2026-09-09

## lane: FullFalcon
branch: main

→ `bd ready` — queue near-empty. loto-iytm (promote staged-lock gate to
blocking) is genuinely time-blocked: needs 14 days of firing counter data
since loto-7oik merged (2026-09-08), so actionable ~2026-09-22, not before.

✓ done
- loto-ea8y (shared-checkout coordination epic) closed 2026-09-09: all 12
  children merged, AC verified against code.
- Cleared the backlog: 6 PRs merged (#217–#222), 9 beads closed. Fixed 2 reviewer findings inline (CodeRabbit bind-var, Codex two-tx atomicity).
- Froze e6r + hnw5 — findings in bead bodies, `human`-labeled, deferred 6mo.

‡ e6r: sqlite OPEN is 3ms, NOT the ~1s CLI cost — daemon needs a pprof on an idle box first. hnw5: bridge infeasible, gap covered by loto tag + native SendMessage.

~ triage-mode: names flaws, merges, walks; froze the judgment-call spikes rather than have me pick architecture.
