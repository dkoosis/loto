# wt-* Harness — Migration Brief

> **Purpose.** Full picture of the parallel-agent SDLC harness currently living in the **trixi** repo (`~/Projects/trixi/scripts/`), written so dk and a migration agent can understand what's being graduated into **loto** and what breaks when it moves. Source of truth for the extraction. Written 2026-06-01.

---

## 0. What this is (and why it's moving)

trixi accreted a full **dev-harness for running multiple Claude-agent sessions against one git repo**. It was born 2026-05-02 (the day parallel agents started colliding) and compounded by a recursion: *parallel agents create friction → each friction gets a script → scripts need coordination (`loto`) → coordination needs lifecycle (`wt-*`) → lifecycle needs enforcement (make gates + a generated rule + git hooks + tests)*. ~21% of trixi's last 60 days of commits touched this harness; none of it touches a nug, an embedding, or the MCP store.

`loto` (the file-lease lock coordinator) already graduated to its own repo (`github.com/dkoosis/loto`, a single Go binary). The `wt-*` worktree-lifecycle scripts are the **same domain** but never graduated. **Decision (dk, 2026-06-01): graduate the harness to loto** so bead and code live together.

These scripts are **barely coupled to trixi internals** — no trixi Go packages, no KG logic. They shell out to generic `git` / `gh` / `bd` / `make check` / `loto` / `gh-poi`. The coupling that *does* exist is to **conventions** (branch prefixes, worktrees-dir naming, `.beads`/`.claude`/`docs` paths, the `make check` green-gate), enumerated in §4.

---

## 1. The components

Nine scripts in `~/Projects/trixi/scripts/`. The first four (`wt-*`) are the core lifecycle; the rest are supporting infra.

| Script | LOC | Role | Destructive? |
|--------|-----|------|--------------|
| `wt-status` | 372 | Inventory + classify every worktree/orphan; emits JSON or human report | no (read-only) |
| `wt-gc` | 207 | Deletion authority — removes merged worktrees/branches, recovers orphans | **yes** (guarded) |
| `wt-land` | 229 | Idempotent land-and-close: push → `make check` → PR → squash-merge → verify → `bd close` | mutates remote + bead |
| `wt-discard` | 161 | Explicit abandonment of a bead's worktree | **yes** (guarded) |
| `hooks-install` | 191 | Writes `.githooks/` shims, sets `core.hooksPath`, injects SessionEnd nag | mutates git config + settings |
| `lint-locked` | 85 | Machine-global mutex around `golangci-lint` so parallel sessions serialize | no |
| `sdlc-lag.sh` | 127 | Process metric: median push-lag per closed bead per week | no (read-only) |
| `check-generated-concurrency-rule` | 79 | Verify/regenerate `concurrency.md` from the protocol doc's `## Compact rule` block | `--write` overwrites the rule |
| `bd-close-epic` | 43 | `bd close` + sync linked `gh issue close` via `gh_issue` metadata | mutates bead + GH issue |

### Lifecycle flow (the 5 layers, per `docs/parallel-sessions-protocol.md`)

```
create:    bd worktree create ../trixi-worktrees/<id> --branch fix/<id>
work:      (agent in worktree; loto leases files; pre-commit/pre-push hooks gate)
land:      scripts/wt-land <id>      # push, make check, PR, squash-merge, verify, bd close
inventory: scripts/wt-status         # local triage; runs at SessionEnd (--warn-only)
gc:        scripts/wt-gc             # gh-poi confirms merge, removes worktree+branch
abandon:   scripts/wt-discard <id>   # then bd close --reason abandoned
recover:   scripts/wt-gc --apply     # orphan dirs → revert stuck beads to open, delete bare dirs
```

---

## 2. Per-script detail

### `wt-status` (372 lines, bash) — inventory + triage
- **What:** Inventories all git worktrees plus orphan dirs in the sibling `../<repo>-worktrees/` dir, classifies each with a verdict, emits a JSON array (`--json`) or one human line per entry.
- **Invoked by:** SessionEnd hook (`bash scripts/wt-status --warn-only`); `wt-gc` as subprocess (`$WT_STATUS_BIN`); `make wt-status-test`; manual.
- **Flags:** `--json`, `--warn-only` (suppress clean lines, force exit 0), `--refresh` (`git fetch --all --prune` first), `--repo-root <p>`, `--worktrees-root <p>`.
- **Deps:** `git`, `gh` (one hoisted `gh pr list --limit 1000 --state all`), `bd` (`bd show <id> --json`), `jq`, `loto` (`loto doctor` in human path).
- **Output:** JSON array, or TSV human lines `VERDICT[STASH-WARN]\t<bead_id>\t<path>`. No mutations.
- **Exit:** `--warn-only`→0 always. Else 0=clean, 1=warnings, 2=dangerous (NO-BEAD / CLOSED-BEAD-NOT-REMOVABLE / ORPHAN / ORPHAN-DIRTY).
- **Gotchas:** Single hoisted `gh` call for all branches. Squash-merge workaround — `READY-TO-GC-CANDIDATE` skips the `ahead_origin_main==0` gate (`trixi-j8mh`). `pwd -P` everywhere to defeat macOS `/var`→`/private/var`.

### `wt-gc` (207 lines, bash) — deletion authority
- **What:** Calls `wt-status --json --refresh`; for each `READY-TO-GC-CANDIDATE` consults `gh-poi`, then removes the clean worktree + local branch (never `--force`). Orphan dirs → revert stuck `in_progress` beads to `open` and `rm -rf` the bare dir.
- **Invoked by:** Manual; `make wt-gc-test`; documented in `concurrency.md`.
- **Flags:** `--dry-run`, `--apply` (required for orphan rollback execution), `--repo-root`, `--worktrees-root`, `--wt-status <path>` (test injection), `-h`.
- **Deps:** `git`, `gh-poi` (`$GH_POI_BIN`, default `gh-poi`), `bd` (`bd update` for rollback), `jq`, `wt-status`.
- **Output:** TSV per action (`removed|skip|would-remove|orphan-removed|fail-*`); stderr summary `removed=N skipped=N errors=N`.
- **Exit:** 0 success, 1 if any errors.
- **Gotchas:** `assert_under_worktrees_root` hard-stop — refuses to delete any path that doesn't canonically resolve under the worktrees dir (symlink/traversal guard). `chmod -R u+w` before `rm -rf` to clear read-only Go mod-cache files. `sudo rm -rf` is last-resort, orphans only.

### `wt-land` (229 lines, bash) — land and close
- **What:** Idempotent 6-step: push branch → `make check` → find-or-create PR → squash-merge → **verify by content diff** against `origin/main` → `bd close`. Resumes from the failed step on re-run.
- **Invoked by:** Manual, typically from inside the bead worktree (dispatched-agent case); `make wt-land-test`.
- **Flags:** `<bead-id>` (positional; branch = `fix/<bead-id>`), `--force` (skip `make check`), `--dry-run`, `--yes` (skip confirms, for agents).
- **Deps:** `git`, `gh` (`pr list/create/merge`), `bd` (`bd close`), `make` (`make check`).
- **Exit:** 0 landed+closed; 1 check-fail / not-landed / bd-close-fail / abort; 2 usage; 3 no branch/worktree.
- **Gotchas:** Verifies by **tree diff** (`git diff --quiet origin/main "$BRANCH"`), not gh's exit code ("gh can report success when the merge did not happen"). Does **not** pass `--delete-branch` (would fail on the active worktree and break the chain before `bd close`); deletion is wt-gc's job. ⚠ **This is the script `trixi-vdud` is filed against** — the content-gate false-negatives once `origin/main` advances past the branch base, so the auto-close rarely fires. Fix direction lives in the bead.

### `wt-discard` (161 lines, bash) — explicit abandonment
- **What:** Abandons a bead's worktree: dirty-state check, optional patch archive, remove worktree, optional branch delete. Does **not** touch bead state (prints `next: bd close <id> --reason abandoned`).
- **Invoked by:** Manual; documented in `concurrency.md`. No make target.
- **Flags:** `<bead-id>`, `--archive-patch <file>`, `--force-with-confirmation`, `--repo-root`, `--yes`, `-h`.
- **Deps:** `git` only.
- **Exit:** 0 discarded; 1 dirty-without-override / not-found / remove-fail; 2 usage.
- **Gotchas:** Refuses to discard a dirty tree without `--force-with-confirmation` or `--archive-patch`. Branch delete needs explicit confirm if pushed (unless `--yes`).

### `hooks-install` (191 lines, bash) — git-hook + SessionEnd wiring
- **What:** Regenerates `.githooks/<name>` shims (each chains `bd hooks run <name>` first, then trixi-specific checks), sets `core.hooksPath=.githooks`, and JSON-merges a `SessionEnd` entry running `wt-status --warn-only` into `.claude/settings.json`.
- **Invoked by:** `make hooks-install`; **`make check` depends on it**; `make hooks-install-test`.
- **Flags:** `--repo-root`, `-h`.
- **Deps:** `git`, `bd` (`bd hooks list/run` at hook runtime), `jq` (settings merge), `find`.
- **Output:** writes `.githooks/`, sets `core.hooksPath` + `hooks.priorPath`, merges `.claude/settings.json`.
- **Gotchas:** Idempotent — shims regenerated from template bytes each run; settings touched only if SessionEnd entry absent. The pre-push shim embeds `TRIXI_ALLOW_DIRTY_PUSH` bypass + `make check`; pre-commit shim calls `check-generated-concurrency-rule`. **This is the deepest trixi-coupling point** — see §4.

### `lint-locked` (85 lines, bash) — golangci-lint mutex
- **What:** Wraps `golangci-lint` behind a machine-global `mkdir`-based mutex so concurrent worktree sessions serialize instead of failing with golangci's exit-3 "another instance running."
- **Invoked by:** **Every** golangci-lint call in trixi's Makefile via `GOLANGCILINT := bash scripts/lint-locked` (covers `make lint/scan/check-pkg/check/report/ci-report`).
- **Deps:** `golangci-lint`, `stat`, `date`, `sleep`. Envvars: `GOLANGCI_LINT_CACHE`, `LINT_LOCK_TIMEOUT` (300s), `LINT_LOCK_STALE` (900s).
- **Coupling:** **None beyond the Makefile `GOLANGCILINT` var.** Lock path derives from the golangci cache dir, not a trixi path. The most portable script in the set.
- **Gotchas:** POSIX `mkdir` mutex (no `flock`). Stale reclaim via atomic `mv`+`rm -rf` (no TOCTOU). Writes notices to `/dev/tty` so they survive stderr→/dev/null in the SARIF pipeline. ⚠ Related to `trixi-rorp` (stale cache + global lock breaking `make check` across sessions).

### `sdlc-lag.sh` (127 lines, bash) — process metric
- **What:** For each closed bead, finds its commit on `origin/main` by message grep, looks up push time from local reflog, reports weekly median/percentile of `push_time − close_time`.
- **Invoked by:** Manual only. No make target, no hook.
- **Deps:** `git`, `jq`, `awk`, `tac`, `date`, `mktemp`.
- **Coupling:** Reads `.beads/issues.jsonl` **directly** (bypasses `bd`), knowing the schema (`._type=="issue"`, `.status`, `.id`, `.closed_at`). Assumes bead IDs appear verbatim in commit messages.
- **Gotchas:** Only accurate on the machine where pushes happened (local reflog). Dual BSD/GNU `date` handling.

### `check-generated-concurrency-rule` (79 lines, bash) — rule generator/checker
- **What:** Extracts the `## Compact rule` fenced block from `docs/parallel-sessions-protocol.md` and verifies (or `--write` regenerates) `.claude/rules/concurrency.md`.
- **Invoked by:** `make check-concurrency-rule`, `make sync-rules` (`--write`), and the **pre-commit shim** (via hooks-install).
- **Deps:** `git`, `awk`, `diff`.
- **Coupling:** Hardcodes `SRC=docs/parallel-sessions-protocol.md`, `DST=.claude/rules/concurrency.md`, the `## Compact rule` heading, and "fix with: make sync-rules".
- **Exit:** 0 in-sync, 1 drift/missing-DST, 2 bad-arg, 3 not-in-repo/missing-SRC/no-fence.

### `bd-close-epic` (43 lines, bash) — bead+GH-issue close sync
- **What:** `bd close <id>` then, if the bead carries `gh_issue=<N>` metadata, `gh issue close <N>`.
- **Invoked by:** Manual; `make bd-close-epic-test`; listed in `make check` prereqs.
- **Deps:** `bd` (`$BD_BIN`), `gh` (`$GH_BIN`), `jq`.
- **Coupling:** Knows the `gh_issue` metadata key and the `.[0]` array shape of `bd show --json`.
- **Gotchas:** Closes bead first, then syncs GH (correct order). `BD_BIN`/`GH_BIN` for test isolation.

---

## 3. How trixi wires it all together

The scripts aren't standalone — trixi's build/session machinery invokes them. **These are the integration points that must be re-pointed (or stubbed) when the scripts move.**

| Integration point | File | What it does |
|---|---|---|
| `make check` prereqs | `Makefile:184` | runs `wt-status-test wt-gc-test wt-land-test bd-close-epic-test check-concurrency-rule` + depends on `hooks-install` |
| golangci wrapper | `Makefile` (`GOLANGCILINT :=`) | routes **all** lint through `bash scripts/lint-locked` |
| concurrency-rule gate | `Makefile:205-209` | `check-concurrency-rule` / `sync-rules` call `check-generated-concurrency-rule` |
| SessionEnd nag | `.claude/settings.json:57` | `"command": "bash scripts/wt-status --warn-only"` |
| git hooks | `.githooks/*` (written by `hooks-install`) | pre-commit → `check-generated-concurrency-rule`; pre-push → `make check` + `TRIXI_ALLOW_DIRTY_PUSH` |
| generated rule | `.claude/rules/concurrency.md` | generated from `docs/parallel-sessions-protocol.md §Compact rule` |
| protocol doc | `docs/parallel-sessions-protocol.md` | the human spec; **source of truth** for the generated rule + script behavior |
| guard-rails ref | `.claude/rules/guard-rails.md` | documents the worktree-per-bead workflow + `wt-gc` post-merge |
| plan docs | `docs/superpowers/plans/trixi-85in-*.md`, `trixi-c4bj-*.md` | the original implementation plans |
| tests | `scripts/test/wt-*_test.sh`, `hooks-install_test.sh` | shell test harnesses, run by the make `*-test` targets |

---

## 4. Coupling map — what breaks if you move a script

Ordered easiest → hardest to extract.

1. **`lint-locked` — trivial.** Zero trixi coupling beyond the `GOLANGCILINT` Makefile var. Move the script; each consuming repo points its own `GOLANGCILINT` at it (or at `loto`-installed `lint-locked` on PATH).

2. **`sdlc-lag.sh` — easy, but reads `.beads/issues.jsonl` directly.** Portable to any beads repo. Only assumption is the JSONL schema + bead-id-in-commit-message convention.

3. **`wt-discard` — easy.** `git`-only. Couplings are *conventions* (branch prefixes, `branch.<name>.bead` config, primary-worktree detection), not trixi paths. Moves clean if conventions are preserved/parameterized.

4. **`bd-close-epic` — easy.** `bd`+`gh`+`jq`. Couples to the `gh_issue` metadata convention. Portable to any beads+GH repo.

5. **`wt-status` / `wt-gc` — moderate.** `git`/`gh`/`bd`/`jq`/`gh-poi`/`loto`. Couplings: worktrees-root naming (`<repo>-worktrees`, already `--worktrees-root`-overridable), branch-prefix taxonomy, the `team_worktree` bead-metadata key, `gh-poi` on PATH. All *conventions/CLIs*, not trixi code. Move clean if the conventions travel with them.

6. **`wt-land` — moderate.** Couples to `fix/` prefix + `make check` as the green-gate + `bd close`. Carries the open **`trixi-vdud`** bug. The `make check` assumption means each consuming repo must have a `make check` (or the gate command must be parameterized).

7. **`check-generated-concurrency-rule` — moderate, but tied to the protocol doc.** Hardcodes `docs/parallel-sessions-protocol.md` → `.claude/rules/concurrency.md`. If the protocol doc graduates to loto, this generator + the `make check-concurrency-rule` gate move with it; trixi either drops the gate or consumes a loto-published rule.

8. **`hooks-install` — hardest. The deepest coupling.** Hardcodes `.beads/hooks/`, `.githooks/`, `.claude/settings.json`, the `SessionEnd` command string, `TRIXI_ALLOW_DIRTY_PUSH`, and embeds `trixi-85in` bead refs in shim comments. It is fundamentally "wire *this repo's* hooks." The realistic outcome: a **generic installer in loto parameterized by repo-root + repo-name + gate-command**, with trixi calling `loto install-hooks --repo-root . --gate 'make check'` (or similar). The pre-commit→`check-generated-concurrency-rule` and pre-push→`make check` steps are trixi-specific and must become injected hooks, not baked-in template bytes.

### The break-on-move blast radius in trixi
Moving the scripts without re-pointing these leaves trixi **broken**:
- `.claude/settings.json:57` SessionEnd → dangling `scripts/wt-status` path.
- `make check` → missing `wt-*-test` / `bd-close-epic-test` / `check-concurrency-rule` targets and the `hooks-install` dep.
- `GOLANGCILINT := bash scripts/lint-locked` → all lint breaks.
- `.githooks/` shims → dangling `scripts/check-generated-concurrency-rule`.
- `.claude/rules/concurrency.md` → orphaned generated file (drift gate gone).

---

## 5. The design fork (decide before executing)

`loto` is a **single Go binary** (`cmd/loto`), no `scripts/` dir. Two ways the harness can live there:

**A. Bash on PATH.** Move the 4–9 bash scripts into `loto/scripts/`, install them on PATH via loto's `install` target. trixi calls bare `wt-land` / `wt-gc` / etc.
- *Pro:* faithful, low-risk, preserves the working scripts + their shell tests verbatim.
- *Con:* loto now ships shell **and** Go; conventions (branch prefixes, worktrees-root, gate-command) stay hardcoded unless parameterized via flags/env.

**B. Go subcommands.** Reimplement as `loto wt status|gc|land|discard`, `loto hooks install`, etc. trixi calls `loto wt …`.
- *Pro:* loto stays one Go binary; the open bugs (`vdud`, `rorp`) get fixed in typed Go with real tests; conventions become explicit config.
- *Con:* a real rewrite (≈1200 lines of careful bash → Go), and the **recursion risk** — this is exactly where a 4-script move becomes another 300 commits. Scope it hard.

> Recommendation on file: graduate as a **bounded epic with a plan** (`requires_plan` — cross-repo, touches a generated rule + hooks + the check gate). Whichever fork, parameterize the trixi-conventions (repo-root, repo-name/worktrees-root, branch-prefix set, gate-command, metadata keys) so loto's harness is repo-agnostic and trixi becomes just one consumer.

---

## 6. Shared idioms (reuse surface — matters most for fork B)

- **Worktrees-root:** `"$(dirname "$rr")/$(basename "$rr")-worktrees"` — derived identically in wt-status + wt-gc; both take `--worktrees-root`.
- **Repo root:** `git rev-parse --show-toplevel` + `--repo-root` override, everywhere.
- **macOS canonicalization:** `cd … && pwd -P` to defeat `/var`→`/private/var` (wt-status, wt-gc).
- **Bead-id from branch:** strip `fix/|feat/|chore/|docs/|spec/|refactor/|test/`, then prefer `branch.<name>.bead` git config (wt-status, wt-discard).
- **`bd show … --json | jq -r '.[0].field'`:** the `.[0]` array shape (wt-status, wt-land, bd-close-epic).
- **Test injection:** binary-path env/flags (`GH_POI_BIN`, `--wt-status`, `BD_BIN`, `GH_BIN`, `GOLANGCI_LINT_CACHE`).
- **Boilerplate:** `set -euo pipefail` + self-extracting `--help` via `sed -n 'X,Yp' "$0"`.

---

## 7. Beads to migrate (trixi → loto)

The open trixi beads that belong to this harness's domain. Migrate **with** the code (don't orphan them):

| trixi id | P | Title | Subject script |
|---|---|---|---|
| `trixi-5qh5` | P1 | Agent `isolation:worktree` flag silently ignored — parallel subagents share one tree (keystone) | dispatch/harness |
| `trixi-vdud` | P2 | `wt-land` content-gate false-negatives when main advances past branch base | `wt-land` |
| `trixi-rorp` | P2 | golangci-lint stale worktree cache + global parallel lock break `make check` | `lint-locked` |
| `trixi-2juh` | P2 | Hooks revert/clobber edits in worktrees + parallel sessions | `hooks-install` / hooks |
| `trixi-7t0u` | P2 | git merge driver for `.beads/issues.jsonl` (auto-resolve by newer `updated_at`) | beads-tooling (judgment call) |

Already migrated: `trixi-r4gm` (loto leases) → lives in loto as `loto-k5el` (+ `.1`/`.2`), closed in trixi. `trixi-h71k`/`c4bj` (the scripts + orphan recovery) are closed/landed.

> Note: `5qh5` is partly **harness** (the Claude Code `Agent` tool's `isolation:worktree` param) not pure wt-*. Its fix may be detect-and-hard-fail rather than own-code. Keep that distinction when planning.

---

## 8. Open questions for migration-claude

1. **Fork A or B** (§5)? Gates the whole plan shape.
2. **Does the protocol doc** (`docs/parallel-sessions-protocol.md`) graduate to loto, stay in trixi, or get split? It's the source of the generated `concurrency.md`.
3. **How does trixi consume the graduated harness** — bare commands on PATH, `loto wt …`, or a thin trixi wrapper? Whatever it is, re-point all of §3.
4. **Parameterization contract:** which conventions become flags/config (worktrees-root ✓ already, branch-prefix set, gate-command, `team_worktree`/`gh_issue` metadata keys, hook step injection)?
5. **Test migration:** the `scripts/test/*_test.sh` shell harnesses move with fork A; fork B needs them rewritten as Go tests.
6. **Don't regress trixi:** the extraction PR must leave trixi's `make check` green and SessionEnd/hooks working — verify in both repos before closing.

---

*Reference: trixi `docs/parallel-sessions-protocol.md` (full spec), `Makefile` (gates), `.claude/rules/concurrency.md` (generated), beads `trixi-85in` (original harness epic).*
