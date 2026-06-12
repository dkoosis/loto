# Workflow

*Dev/PR/CI conventions for the loto repo. Durable — migrated from recurring boot.md traps.*

‡ Go symbol questions → `snipe` (def/refs/callers/pack/impact/tests) before rg/Grep. rg = non-symbol text only.

‡ **store Open / race-path fixes → ALWAYS via PR, never direct-to-main.** linux `-race` runs only on CI, not local macOS. Even a no-op refactor touching `internal/store/*` or `internal/identity/registry.go` goes through a PR (#170 honored this).

‡ **Parallel sessions are routine here.** `git fetch` before judging any branch's state — a branch that looks like cruft may be live unmerged work. Verify with `git cherry main origin/<branch>`: `+` = unapplied, `-` = already applied. (#166–169 merged out from under a session mid-review.)

‡ **CI = self-hosted serial runners** (`mac-loto`, `trixi-loto`), matrix linux+macos, each runs `go test -race ./...`. A burst of merges backlogs the queue ~15–20 min — that's lag, not breakage. Check `gh api repos/dkoosis/loto/actions/runners` for busy state.

- docs(boot) / docs-only commits → direct to main is fine. test-only (non-store/identity) → direct fine.
- phantom-lint: golangci can flag findings in `.claude/worktrees/agent-*` copies — verify against real `internal/` source; `golangci-lint cache clean` if stale.

‡ **Tests are stdlib-only — no testify/go-cmp.** Convention is plain `t.Errorf`/`errors.Is` (φ `internal/domain/target_test.go`). Reject PRs that add assertion-helper deps/packages; fold their value in stdlib style. (#176 dragged in testcmp/testrequire clones — closed, rewritten.)
‡ **Arch linter rejects black-box `*_test` self-import.** `package foo_test` importing its own `loto/internal/foo` trips `make check`'s dependency-violation gate. Use internal `package foo` for in-package tests.
