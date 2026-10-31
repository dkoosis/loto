#!/usr/bin/env bash
# Tests for tree-hook.sh (mn-wois.12): failing open must reach the model as
# additionalContext JSON, once per session and only on pre; a real refusal from
# a current loto still exits 2 with the holder on stderr.
#
# PATH is cut to the system dirs, plus a stub `loto` where a case wants one.
set -u

HOOK="$(cd "$(dirname "$0")" && pwd)/tree-hook.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail=0

# run <session> <verb> <path-prefix> [agent]: sets out, err, rc from one hook call.
run() {
  out=$(printf '{"session_id":"%s","tool_name":"Bash","tool_use_id":"c","agent_id":"%s"}\n' "$1" "${4:-}" |
    TMPDIR="${HOOK_TMP:-$WORK/tmp}" PATH="${3:+$3:}/usr/bin:/bin" /bin/bash "$HOOK" "$2" 2>"$WORK/stderr")
  rc=$?
  err=$(cat "$WORK/stderr")
}

check() { # check <name> <condition result 0|1> <detail>
  if [ "$2" -ne 0 ]; then
    echo "✗ tree-hook: $1: $3"
    fail=1
  fi
}

# is_context_json <text> <want>: one line, the hook JSON shape, holds want.
is_context_json() {
  case "$1" in
    '{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"'*"$2"*'"}}') ;;
    *) return 1 ;;
  esac
  [ "$(printf '%s\n' "$1" | wc -l | tr -d ' ')" = 1 ] || return 1
  if command -v jq >/dev/null 2>&1; then
    printf '%s' "$1" | jq -e '.hookSpecificOutput.additionalContext' >/dev/null || return 1
  fi
}

mkdir -p "$WORK/tmp" "$WORK/stale" "$WORK/current"
cat >"$WORK/stale/loto" <<'STUB'
#!/bin/sh
echo "unknown command: $1" >&2
exit 2
STUB
cat >"$WORK/current/loto" <<'STUB'
#!/bin/sh
[ "$2" = --help ] && exit 0
echo "✗ hook: refused path=a.go holder=peer" >&2
exit 2
STUB
chmod +x "$WORK/stale/loto" "$WORK/current/loto"

# Loto not on PATH: the notice is JSON on stdout, exit 0.
run s-missing pre ""
check "missing: exit" "$rc" "want 0, got $rc"
is_context_json "$out" "loto is not on PATH"
check "missing: JSON" $? "stdout=$out"

# The second call of that session is silent; another session hears it.
run s-missing pre ""
[ -z "$out" ] && [ "$rc" = 0 ]
check "missing: once per session" $? "rc=$rc stdout=$out"
run s-other pre ""
is_context_json "$out" "loto is not on PATH"
check "missing: another session" $? "stdout=$out"

# A subagent hearing it first leaves the main thread's notice unspent.
run s-sub pre "" agent-1
is_context_json "$out" "loto is not on PATH"
check "missing: subagent hears" $? "stdout=$out"
run s-sub pre ""
is_context_json "$out" "loto is not on PATH"
check "missing: main thread still hears" $? "stdout=$out"

# A marker that cannot be written repeats the notice rather than silencing it.
: >"$WORK/not-a-dir"
HOOK_TMP="$WORK/not-a-dir"
run s-unwritable pre ""
is_context_json "$out" "loto is not on PATH"
check "unwritable marker: first" $? "stdout=$out"
run s-unwritable pre ""
is_context_json "$out" "loto is not on PATH"
check "unwritable marker: repeats" $? "stdout=$out"
unset HOOK_TMP

# Post is not PreToolUse: silent, and it does not spend the session's notice.
run s-post post ""
[ -z "$out" ] && [ "$rc" = 0 ]
check "missing: post silent" $? "rc=$rc stdout=$out"
run s-post pre ""
is_context_json "$out" "loto is not on PATH"
check "missing: post keeps the notice" $? "stdout=$out"

# A binary that predates the hook verb fails open too, not exit 2.
run s-stale pre "$WORK/stale"
check "stale: exit" "$rc" "want 0, got $rc"
is_context_json "$out" "predates the hook verb"
check "stale: JSON" $? "stdout=$out"

# A current loto's refusal passes through: exit 2, the holder on stderr.
run s-blocked pre "$WORK/current"
[ "$rc" = 2 ]
check "refusal: exit" $? "want 2, got $rc"
case "$err" in *holder=peer*) ;; *) check "refusal: holder" 1 "stderr=$err" ;; esac

[ "$fail" -eq 0 ] && echo "✓ tree-hook: fail-open notices reach the model; refusals still block"
exit "$fail"
