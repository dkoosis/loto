# Boot
updated: 2026-06-01

→ Ready queue EMPTY · 0 open PRs · tree clean · build/vet/lint/race green. No queued work — next session pulls fresh from `bd ready` or a new ask.

✓ done
- #162–#165 (loto-u7b7/0gsu/qv91/3c7y) merged, beads closed. `go install ./cmd/loto` → LOTO_AGENT_ID populates.
- #166–#169 (loto-l3as/qqy5/8yst/pduc) — the queued `/team impl 4` fixes; assessed + validated here, merged by a parallel session mid-review, beads closed. qqy5 flake fixed (ctx-cancel now prioritized over poll timer).
- #170 (06-01 cleanup): /simplify on #166-169 → `permAfterNlinkCheck` (dedup the open-fd Nlink>1 TOCTOU guard across stripWrite/restoreWrite), `injectHardlinkOnce` + `commandsForEvent` test helpers. No behavior change, both-platform CI green. /clean: vet+golangci 0 issues codebase-wide.
- hygiene: pruned 4 stale loto worktrees (gone from disk), deleted temp integration + chore branches. No stashes/worktrees/open PRs left.

‡ traps
- store Open/race-path fixes → ALWAYS via PR, never direct-to-main: linux `-race` only runs on CI, not local macOS. #170 honored this even for a no-op chmod.go refactor.
- parallel sessions are routine here — `git fetch` before judging branch state. A branch that looks like cruft may be live unmerged work; verify `git cherry main origin/<b>` (`+` = unapplied, `-` = already applied). #166-169 merged out from under this session while it reviewed.
- CI = self-hosted serial runners (mac-loto, trixi-loto). A burst of merges backlogs the queue ~15-20min; that's lag, not breakage. Linux+macos matrix, each runs `go test -race ./...`.
