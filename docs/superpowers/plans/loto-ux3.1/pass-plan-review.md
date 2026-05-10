# pass-plan-review — loto-ux3.1

reviewer: feature-dev:code-reviewer (subagent)
verdict: **approve-with-revisions**

## P0 (blocking)

### `loto doctor --repair` destroys acquire'd holds

`doctor.go:examineTagPair` (lines 273–288) classifies a free flock + present tag as `DriftStaleTag` and removes the tag when `--repair` runs. AcquirePath releases the flock by design after writing the tag — so every acquire'd hold presents exactly as `DriftStaleTag`. `loto doctor --repair` will silently destroy persistent holds the moment it runs.

The plan's step 8 (build order) mentions probing `loto status` but frames it as a display concern. The doctor is a **correctness** concern: it actively destroys what the bead creates. The plan's "Files NOT touched" list (plan.md line 131) explicitly excludes `doctor.go` — that constraint must be reconsidered, or a follow-up bead for the doctor fix must be filed and the P0 acknowledged before shipping AcquirePath.

The fix: `examineTagPair` must check `tag.ExpiresAt` before classifying. When the flock is free but the tag has a non-zero, unexpired `ExpiresAt`, the correct classification is "live record-tier hold" — not `DriftStaleTag`. This requires touching `doctor.go` and adding a test parallel to `TestDoctorSoftStaleHeld` (doctor_test.go:236) that confirms a valid acquire'd hold is not destroyed.

## P1 (must-fix-before-impl)

### `tag.IsExpired()` — method does not exist

plan.md line 29 calls `!tag.IsExpired()`. The codebase exposes `(*Tag).SoftStale()` at loto.go:118 for this predicate. Different name, same concept. If the implementer follows the plan literally, they either invent a redundant method or get a compile error. The plan must say `SoftStale()` — or explicitly declare `IsExpired()` as a new alias and note that addition.

### `EmitLLMReleased` name collision — compile error

plan.md line 68 / step 9 proposes `EmitLLMReleased(w, target string)` as a new function. `EmitLLMReleased` already exists at `internal/render/llm.go:299` with signature `(w io.Writer, agent string, n int, errs []string)`. Go does not allow function overloading; this will not compile. The path-form emitter needs a distinct name. Suggested: `EmitLLMReleasedPath(w io.Writer, target string) error`.

### `lazyReapTag(tagPath, respectTTL bool)` — bool parameter is unnecessary indirection

plan.md line 42 proposes a `respectTTL bool` parameter. Risk item 1 (line 135) then entertains passing `false` for the global path — contradicting the body text's "existing call sites pass `true`". The flag is not needed: `lazyReapTag` can consult the tag's own `ExpiresAt` directly. The correct guard is `PID dead AND (ExpiresAt.IsZero() OR SoftStale())`. No bool, no divergent call-site behavior, no global-path exception debate. Remove the parameter and encode the logic unconditionally.

## P2

### `releaseCmd()` restructuring not described

Current `releaseCmd()` (main.go:339–341) unconditionally exits 2 if `!allMine`. Extending it to accept a positional path requires reworking the `RunE` conditional, updating `Use`/`Long`, and handling mutual-exclusion of `--all-mine` and a positional arg. The plan states the expected behavior but doesn't describe the structural change. Worth a line.

### Status/doctor indistinguishability probe scope is too narrow

Plan step 8 hedges the probe to `loto status` only. Given the P0, the probe must include `loto doctor` behavior. `TestDoctorSoftStaleHeld` (doctor_test.go:236) is the structural template; a parallel test for the acquire'd case must be part of the same pass.

## ✓ what the plan got right

- **ExpiresAt check placement in TryFileLock is correct.** Placing the guard *before* `lazyReapTag` is essential and non-obvious; the plan gets it right.
- **`--wait` minimal loop decision.** `pollAcquire` returns `*ActiveLock`, AcquirePath returns `*Tag`. Generalizing would require type-param/adapter. Local loop avoids scope creep.
- **No reservation scan in AcquirePath** — defensible (but see arch-fit P1: reconsider for hook-adapter context).
- **Naming `AcquirePath`** — defensible, mirrors `AcquireGlobal`.
- **Acceptance mapping is complete** — every bead bullet has a named test.
- **Exit code 3 for `--wait` timeout** — aligned with NS exit-code table.
- **"Files NOT touched" list** — makes scope creep visible (the P0 forces revision, but better to discover here than mid-impl).

## north-star recenter

> "Five Claude Code sessions, same repo, different subtrees, each spawning subagents. All editing files. Today they clobber each other or panic on unexpected diffs. loto exists so any Claude can answer one question fast: 'Is it safe for me to edit this path right now, and if not, who's on it?'"

The record-tier addition extends the answer to the hook-adapter use case — signaling "I'm about to edit this" across a process boundary without holding a foreground process. North-star aligned and necessary for loto-ux3.2. The P0 is an integration hole in an existing tool; not a direction problem. Fix the doctor in the same pass and the bead lands cleanly.
