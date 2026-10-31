#!/usr/bin/env bash
# tree-hook.sh <pre|post> — run `loto hook <verb>` for the plugin's tree-state
# hooks (stdin is the harness event, passed through).
#
# ‡ Fail open when loto is missing or predates the `hook` verb. A bare
# `loto hook pre` against a stale binary exits 2 ("unknown command"), and exit 2
# in a PreToolUse hook blocks the tool call — Bash included, so the session
# could not run `make install` to repair it. A real refusal from a current
# loto (exit 2 with the holder named) still passes through unchanged.
#
# Failing open tells the model, as additionalContext JSON: Claude Code drops a
# PreToolUse hook's plain stdout and stderr at exit 0. Once per thread, because
# this runs on every tool call: a subagent shares its parent's session id, so
# the marker also carries agent_id, or a subagent would leave the main thread
# untold. No jq: this path must work on a bare machine.
verb=${1:-}
case "$verb" in pre | post) ;; *) exit 0 ;; esac

# event_field <name>: a top-level string of the event in $event.
event_field() {
  printf '%s\n' "$event" | LC_ALL=C sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1
}

# notice_once <key> <text>: text holds no double quote or backslash.
notice_once() {
  [ "$verb" = pre ] || exit 0
  event=$(cat)
  session_id=$(event_field session_id)
  agent_id=$(event_field agent_id)
  if [ -n "$session_id" ]; then
    dir="${TMPDIR:-/tmp}/loto-hook-notice"
    mkdir -p "$dir" 2>/dev/null
    # O_EXCL via noclobber: the first caller creates it, the rest are refused.
    (set -o noclobber; : >"$dir/${session_id//[^A-Za-z0-9_-]/_}.${agent_id//[^A-Za-z0-9_-]/_}.$1") 2>/dev/null || exit 0
  fi
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"%s"}}\n' "$2"
  exit 0
}

off="Edit and Write calls are not checked against peers' locks and no tree reports arrive this session."
command -v loto >/dev/null 2>&1 || notice_once missing "⚠ loto is not on PATH. $off Run make install in the loto repo."
loto hook --help >/dev/null 2>&1 || notice_once stale "⚠ the loto on PATH predates the hook verb. $off Run make install in the loto repo."
exec loto hook "$verb"
