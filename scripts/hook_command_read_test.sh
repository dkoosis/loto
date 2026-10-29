#!/usr/bin/env bash
# No hook loto ships reads the Bash command string (loto-j88z). Bash is not
# gated (docs/write-gate.md): its writes fall to H2 detection, and the parser
# that tried otherwise had 12 false refusals for 1 prevented incident. This is
# the shell half of that check; internal/cli/hook_tooth_test.go is the Go half.
#
# A hook body is any file under hooks/ or .githooks/, or a non-test script
# under scripts/ (hooks/hooks.json runs those). Each is grepped for the tool
# input's command field, in jq's dotted form or a bracketed one
# (.tool_input["command"], d["tool_input"]["command"]).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
Q='["'"'"']'
PATTERN="tool_input$Q?\\]?[[:space:]]*(\\.|\\[[[:space:]]*$Q)command"

# hook_bodies ROOT prints each hook body under ROOT, one per line.
hook_bodies() {
  local root=$1
  for d in hooks .githooks; do
    [ -d "$root/$d" ] && find "$root/$d" -type f
  done
  for f in "$root"/scripts/*.sh; do
    [ -f "$f" ] || continue
    case "$f" in *_test.sh) continue ;; esac
    echo "$f"
  done
}

# readers ROOT prints each hook body under ROOT that reads the command string.
readers() {
  hook_bodies "$1" | while IFS= read -r f; do
    grep -Eq "$PATTERN" "$f" && echo "${f#"$1"/}"
  done
}

fail=0

# The check must go red on a reader, in each place a hook body lives.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/hooks" "$WORK/.githooks/hooks.d" "$WORK/scripts"
printf '%s\n' 'cmd=$(jq -r ".tool_input.command")' >"$WORK/scripts/gate.sh"
printf '%s\n' 'd["tool_input"]["command"]' >"$WORK/.githooks/hooks.d/x"
printf '%s\n' "jq '.tool_input[\"command\"]'" >"$WORK/hooks/pre.sh"
printf '%s\n' 'jq -r .tool_input.command' >"$WORK/scripts/gate_test.sh"
got="$(readers "$WORK" | sort | tr '\n' ' ')"
want=".githooks/hooks.d/x hooks/pre.sh scripts/gate.sh "
if [ "$got" != "$want" ]; then
  echo "✗ hook_command_read: fixture readers = '$got', want '$want'"
  fail=1
fi

# The repo itself has none.
got="$(readers "$ROOT")"
if [ -n "$got" ]; then
  echo "✗ hook_command_read: these hooks read the Bash command string; loto gates no Bash command (docs/write-gate.md):"
  printf '  %s\n' $got
  fail=1
fi

[ "$(hook_bodies "$ROOT" | wc -l)" -gt 0 ] || {
  echo "✗ hook_command_read: found no hook bodies; the check graded nothing"
  fail=1
}

[ "$fail" -eq 0 ] && echo "✓ hook_command_read: no hook reads the command string"
exit "$fail"
