# Boot
updated: 2026-09-09

## lane: FullFalcon
branch: main

→ `bd ready`. Staged-lock gate promotion (loto-iytm): its window opened
2026-09-09T14:48Z (sdlc #456 wired the hook), so no promotion read before
2026-09-23. Samples live in `bd comments loto-iytm`; the firing row rotates
out of the 1000-row events table, so take the next sample before relying on
the count. Its blocker is the beacon-only created-file bug: `bd dep list loto-iytm`.

✓ done
- loto-ea8y (shared-checkout coordination epic) closed 2026-09-09: all 12
  children merged, AC verified against code.
- Cleared the backlog: 6 PRs merged (#217–#222), 9 beads closed. Fixed 2 reviewer findings inline (CodeRabbit bind-var, Codex two-tx atomicity).
- Froze e6r + hnw5 — findings in bead bodies, `human`-labeled, deferred 6mo.

‡ e6r: sqlite OPEN is 3ms, NOT the ~1s CLI cost — daemon needs a pprof on an idle box first. hnw5: bridge infeasible, gap covered by loto tag + native SendMessage.

~ triage-mode: names flaws, merges, walks; froze the judgment-call spikes rather than have me pick architecture.
