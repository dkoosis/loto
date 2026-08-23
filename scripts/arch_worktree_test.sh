#!/usr/bin/env bash
# arch_worktree_test.sh — the .claude exclude must hide agent worktrees and
# nothing else (loto-t5ns).
#
# ‡ An exclusion is only safe if it is proven narrow. Muting .claude stops
# ~100 phantom findings from agent worktrees, and the same edit could as
# easily mute a real violation and go unnoticed for months — a linter nobody
# can trust is worse than no linter. So this asserts BOTH halves: a planted
# violation inside a worktree-shaped path stays silent, and the same
# violation in real source is still reported.
#
# ‡ Everything happens in a THROWAWAY GIT WORKTREE, never the caller's
# checkout. This repo's premise is several agents sharing one tree, so a
# script that plants a non-compiling file in internal/ — even for one second,
# even guarded by a cleanliness check — can break a peer's build for reasons
# nothing in their session explains. That is the failure loto exists to
# prevent, and shipping it in loto's own tooling was the first draft of this
# file (Codex #288). A cleanliness snapshot cannot fix it either: it detects
# edits, not that no peer is mid-BUILD.
#
# ‡ Runs the same invocation `make arch` gates on — the installed
# go-arch-lint, verdict read from ArchHasWarnings — not a convenient
# equivalent. An earlier draft used `go tool go-arch-lint --project-path`,
# which reports findings `make arch` does not, so the script failed while the
# gate was correct.
set -uo pipefail
cd "$(dirname "$0")/.."

say() { printf '%s\n' "$*"; }

if ! command -v go-arch-lint >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  say "⚠ go-arch-lint or jq missing; skipping arch-worktree-scope checks"
  exit 0
fi

probe_root="$(mktemp -d "${TMPDIR:-/tmp}/loto-archprobe-XXXXXX")" || {
  say "✗ arch-worktree-scope: could not create probe dir"
  exit 1
}
probe="$probe_root/wt"
cleanup() {
  git worktree remove --force "$probe" >/dev/null 2>&1
  rm -rf "$probe_root"
}
trap cleanup EXIT

if ! git worktree add --detach "$probe" HEAD >/dev/null 2>&1; then
  say "✗ arch-worktree-scope: could not create the probe worktree"
  exit 1
fi
# The probe worktree is checked out at HEAD, so an uncommitted .go-arch-lint.yml
# would not be the config under test. Copy the working-tree one — the point is
# to prove THIS exclude list, not the last committed one.
cp .go-arch-lint.yml "$probe/.go-arch-lint.yml" || {
  say "✗ arch-worktree-scope: could not stage the config under test"
  exit 1
}

# A back-edge the layering forbids: internal/domain must import nothing
# internal (.go-arch-lint.yml documents domain -> ∅).
violation='package domain

import _ "loto/internal/store"
'

# arch_warns echoes true/false, or "error" when the tool or its JSON failed.
# A tool failure must never read as "the violation was reported" — that would
# let a transient error green the narrowness check without proving anything
# (Codex #288).
arch_warns() {
  local out verdict
  # ‡ The exit code is deliberately ignored, exactly as the Makefile's arch
  # recipe ignores it: go-arch-lint exits NON-ZERO when it finds warnings, so
  # treating that as a tool failure reported "error" for the very case the
  # check exists to detect. The JSON is the verdict; only unparseable or
  # missing JSON is a real failure.
  out=$(cd "$probe" && go-arch-lint check --json 2>/dev/null)
  verdict=$(printf '%s' "$out" | jq -r '.Payload.ArchHasWarnings' 2>/dev/null)
  case "$verdict" in true|false) echo "$verdict" ;; *) echo error ;; esac
}

plant() {
  mkdir -p "$(dirname "$1")" && printf '%s' "$violation" > "$1"
}

fail=0

# 1. A violation inside a worktree-shaped path must NOT be reported.
under_claude="$probe/.claude/worktrees/agent-probe/internal/domain/probe_violation.go"
if ! plant "$under_claude"; then
  say "✗ arch-worktree-scope: could not write $under_claude"
  exit 1
fi
case "$(arch_warns)" in
  false)  say "✓ excluded=.claude planted=.claude/worktrees/agent-probe/... result=silent" ;;
  true)   say "✗ excluded=.claude planted=.claude/worktrees/agent-probe/... result=reported"
          say "ℹ the exclude is not covering agent worktrees; make check goes red whenever an agent runs"
          fail=1 ;;
  *)      say "✗ excluded=.claude result=tool-error"; fail=1 ;;
esac
rm -rf "$probe/.claude"

# 2. The SAME violation in real source MUST be reported.
real="$probe/internal/domain/zz_arch_probe_violation.go"
if ! plant "$real"; then
  say "✗ arch-worktree-scope: could not write $real"
  exit 1
fi
case "$(arch_warns)" in
  true)   say "✓ scope=internal/domain planted=internal/domain/zz_arch_probe_violation.go result=reported" ;;
  false)  say "✗ scope=internal/domain planted=internal/domain/zz_arch_probe_violation.go result=silent"
          say "ℹ the exclude is muting real source — a layering violation would ship unnoticed"
          fail=1 ;;
  *)      say "✗ scope=internal/domain result=tool-error"; fail=1 ;;
esac

if [ "$fail" -ne 0 ]; then
  say "✗ arch-worktree-scope checks=2 failed"
  exit 1
fi
say "✓ arch_worktree_test.sh: 2 checks passed"
