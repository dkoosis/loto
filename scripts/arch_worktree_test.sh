#!/usr/bin/env bash
# arch_worktree_test.sh — the .claude exclude must hide agent worktrees and
# nothing else (loto-t5ns).
#
# ‡ An exclusion is only safe if it is proven narrow. Muting .claude stopped
# ~100 phantom findings from agent worktrees, and the same edit could just as
# easily have muted a real violation and gone unnoticed for months — a linter
# nobody can trust is worse than no linter. So this asserts BOTH halves: a
# planted violation inside a worktree stays silent, and a planted violation in
# real source still fails.
#
# ‡ Runs the SAME invocation `make arch` gates on — the installed
# go-arch-lint plus the ArchHasWarnings assertion — not a convenient
# equivalent. Written first with `go tool go-arch-lint --project-path .`,
# which reports findings `make arch` does not, so the script failed while the
# gate was correct. A guard that does not exercise the real gate proves
# nothing about it.
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
say() { printf '%s\n' "$*"; }

# arch_clean mirrors the Makefile's arch recipe exactly (Makefile:107-108):
# the installed binary, JSON out, verdict read from ArchHasWarnings.
arch_clean() {
  local out
  out=$(go-arch-lint check --json 2>/dev/null) || return 1
  printf '%s' "$out" | jq -e '.Payload.ArchHasWarnings == false' >/dev/null 2>&1
}

if ! command -v go-arch-lint >/dev/null 2>&1; then
  say "⚠ go-arch-lint not installed; skipping arch-worktree-scope checks"
  exit 0
fi

# A back-edge the layering forbids: internal/domain must import nothing
# internal (.go-arch-lint.yml documents domain -> ∅).
violation='package domain

import _ "loto/internal/store"
'

planted_under_claude=".claude/worktrees/arch-probe/internal/domain/probe_violation.go"
planted_real="internal/domain/zz_arch_probe_violation.go"
cleanup() { rm -rf ".claude/worktrees/arch-probe" "$planted_real"; }
trap cleanup EXIT

# 1. A violation inside a worktree-shaped path must NOT be reported.
mkdir -p "$(dirname "$planted_under_claude")"
printf '%s' "$violation" > "$planted_under_claude"
if arch_clean; then
  say "✓ excluded=.claude planted=$planted_under_claude result=silent"
else
  say "✗ excluded=.claude planted=$planted_under_claude result=reported"
  say "ℹ the exclude is not covering agent worktrees; make check will be red whenever an agent runs"
  fail=1
fi
rm -rf ".claude/worktrees/arch-probe"

# 2. The SAME violation in real source must still be reported.
#
# ‡ This half PLANTS A FILE IN REAL SOURCE, and this repo's whole premise is
# several agents sharing one checkout — a peer building during the ~1s window
# would see a package that does not compile. So it runs only when the target
# directory is otherwise clean, which is true in CI (exclusive tree) and when
# nobody is mid-edit locally. Skipping is announced, never silent: a check
# that quietly tests half of what it claims is the thing this script exists
# to prevent.
if [ -n "$(git status --porcelain internal/domain 2>/dev/null)" ]; then
  say "⚠ scope=internal/domain result=skipped reason=working-tree-dirty"
  say "ℹ the narrowness half plants a file in real source; it is skipped rather than risk a peer's build on a shared checkout"
  say "✓ arch_worktree_test.sh: 1 of 2 checks ran"
  exit 0
fi
printf '%s' "$violation" > "$planted_real"
if arch_clean; then
  say "✗ scope=internal/domain planted=$planted_real result=silent"
  say "ℹ the exclude is muting real source — a layering violation would ship unnoticed"
  fail=1
else
  say "✓ scope=internal/domain planted=$planted_real result=reported"
fi
rm -f "$planted_real"

if [ "$fail" -ne 0 ]; then
  say "✗ arch-worktree-scope checks=2 failed"
  exit 1
fi
say "✓ arch_worktree_test.sh: 2 checks passed"
