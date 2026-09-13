# Boot
updated: 2026-09-13

## lane: FullFalcon
branch: main

→ `bd ready`. Staged-lock gate promotion (loto-iytm): its sample clock
RESTARTED 2026-09-13T03:12Z — every unheld-file row before that was the
created-file admission bug (#349), not the gate. Read the count no earlier
than 2026-09-23 from `loto events` on a binary ≥ 6f18c82; guard overrides now
leave `guard_override` rows too (#352), the second signal iytm's Givens name.

✓ done 2026-09-13 (one night, ten loto PRs + two sdlc, dk merged none by hand)
- #349 Write-creates-file admitted holding no lock (loto-9zcq).
- #350 a /team subagent's stamped gate beacon is not foreign at admission;
  `check --gate --staged` exempts a same-session sibling beacon
  (gateDecideStaged, the loto-xwod carve-out on the commit leg) (loto-0z24).
- #351 ref guard: one process is one peer — `/clear` rotates the session id
  but not the pid, and the old record read as a live peer (loto-2jgn).
- #352 LOTO_GUARD_OVERRIDE=1 writes one events row naming the guard (loto-mh07).
- loto-u7c5 closed invalid: git sends old=0 new=0 for every `branch -D`; the
  classifier was right.

- #354 ref guard admits a worktree being born (its HEAD.lock is the sole
  claimant of the row's target, unborn, unoccupied) so tool-ship works with a
  live peer; #357/#359 the collision message names only the stale admin dir,
  rm -rf of it first, unlock && remove as the alternative (loto-w0sx, y71g, tezw).
- #355/#358 moved-locks check treats a zero old or new head as a birth, not a
  fail-open diff (loto-ay2k, 5q2m). #356 `loto doctor` reports a worktree dir
  stuck mid-birth; never reaps it (loto-rode).
- sdlc #483 tool-ship deletes the branch it created when worktree add fails;
  sdlc #484 a second ship onto an existing PR branch three-way merges into a
  new commit (loto-uxa0, dbc8). Reach sessions on the next plugin release.

‡ Codex review quota ran out at 03:58Z; every later PR got an independent
grader (sonnet for routine, opus for the guard's admission path) before merge —
verdicts and reproductions are in each bead's comments. Two graders reproduced
real holes (#354 F1, #357 B1) that green tests did not cover.
‡ Backlog at close: empty. loto-iytm is DEFERRED until 2026-09-23 (`bd undefer loto-iytm` to resume; `bd ready` hides it until then).

~ dk mode: one-word prompts ("continue", "narrower"), chose models by cost —
routine work on sonnet, hard on opus; merged nothing himself.
