# Review bundle — manifest

*What to send with `PROMPT.md` for an architectural / strategic review of loto. Everything here is either in this folder or at the in-repo path named. Files copied from outside the repo carry a provenance header and are snapshots — the source of truth is named in that header.*

## Tier 1 — send always (~790 lines)

| File | Lines | What it settles |
|---|---|---|
| `docs/review/PROMPT.md` | — | The review request itself. PART A is self-contained; PART B fixes the answer shape so reviewers are comparable. |
| `docs/DESIGN.md` | 402 | The contract: invariants, non-goals, "what we are not building", smell tests. Prompt sections 6, 7, and 9 are graded against this. |
| `README.md` | 193 | The concrete surface — nine verbs, worked lock/blocked output, the coordination-layer table. |
| `docs/review/git-gate-plan.md` | 177 | The active epic. Without it, prompt section 10 (same product or a second one?) is unanswerable. |
| `ROADMAP.md` | 14 | One epic, and where it points. |

## Tier 2 — the enforcement boundary (~450 lines)

The design's weakest joint, and the half a reader cannot infer from prose. Include whenever the reviewer will engage with prompt sections 6 or 9.

| File | Lines | What it settles |
|---|---|---|
| `docs/review/pre-tool-use.sh` | 387 | The harness gate. This is the *other side* of the boundary — it lives in the `loto@sdlc` plugin, not this repo, and without it the reviewer sees only loto's half of enforcement. |
| `docs/review/hooks.json` | 25 | How that gate is wired as a `PreToolUse` hook. |
| `docs/review/repo-session-hooks.json` | 36 | Snapshot of this repo's `.claude/settings.json` — where `LOTO_PID` gets exported (liveness depends on it) and where `SessionEnd` releases locks. |

## Tier 3 — code, for reviewers with context to spare (~1,400 lines)

Skip for humans. For an LLM, context is nearly free and these let it check behavior the docs only assert.

| File | Lines | What it settles |
|---|---|---|
| `internal/cli/cmd_check_gate.go` | 238 | loto's side of the enforcement boundary. |
| `internal/store/locks_acquire.go` | 373 | The acquire transaction and in-line dead-holder reclaim. "Liveness-primary, TTL backstop" is true here or nowhere. |
| `internal/domain/records.go` | 207 | What a lock actually is. The cheapest possible read of the data model. |
| `internal/gate/envelope.go` | 336 | git-gate's correctness model — the envelope binding a proposed change to the state it was built against. |
| `internal/gate/admission.go` | 262 | The verdict function. Useless without `envelope.go`; send both or neither. |

Optional: `internal/domain/target.go` if you want path canonicalization scrutinized — symlink aliasing hides there.

Deliberately excluded: `internal/identity/registry.go` (901 lines; its package doc states the contract), the rest of `internal/cli/*` (surface plumbing), `internal/store/store.go` (schema is already in `DESIGN.md`), and `.claude/rules/*` (house style, not architecture).

## For humans

`PROMPT.md` PART A plus `README.md`, then the short form at the bottom of the prompt. Attaching 400 lines of design contract to someone doing you a favor lowers the odds they answer at all.

## Cover note to include

> DESIGN.md is the contract, ROADMAP.md the direction — disagree with either.
