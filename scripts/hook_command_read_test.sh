#!/usr/bin/env bash
# No hook loto ships reads the Bash command string (loto-j88z). Bash is not
# gated (docs/write-gate.md): its writes fall to H2 detection, and the parser
# that tried otherwise had 12 false refusals for 1 prevented incident. This is
# the shell half of that check; internal/cli/hook_tooth_test.go is the Go half.
#
# A hook body is any file under hooks/, .githooks/ or scripts/, at any depth
# and with or without an extension, except a *_test.sh (hooks/hooks.json runs
# scripts from scripts/). Each is grepped for the tool
# input's command field: read straight off tool_input, dotted or bracketed
# (.tool_input.command, d["tool_input"]["command"]), or, in a file that
# touches tool_input at all, a command field read anywhere
# (.tool_input | .command, ti = d["tool_input"]; ti.get("command")).
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
Q='["'"'"']'
PATTERN="tool_input$Q?\\]?[[:space:]]*(\\.|\\[[[:space:]]*$Q)command"
FIELD="\\.command([^[:alnum:]_]|\$)|\\[[[:space:]]*$Q""command$Q[[:space:]]*\\]|get\\([[:space:]]*$Q""command$Q"

# hook_bodies ROOT prints each hook body under ROOT, one per line.
hook_bodies() {
  local root=$1
  for d in hooks .githooks; do
    [ -d "$root/$d" ] && find "$root/$d" -type f
  done
  [ -d "$root/scripts" ] && find "$root/scripts" -type f ! -name '*_test.sh'
  return 0
}

# readers ROOT prints each hook body under ROOT that reads the command string.
readers() {
  hook_bodies "$1" | while IFS= read -r f; do
    if grep -Eq "$PATTERN" "$f" ||
      { grep -q tool_input "$f" && grep -Eq "$FIELD" "$f"; }; then
      echo "${f#"$1"/}"
    fi
  done
}

fail=0

# The check must go red on a reader, in each place a hook body lives.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/hooks" "$WORK/.githooks/hooks.d" "$WORK/scripts/sub"
printf '%s\n' 'cmd=$(jq -r ".tool_input.command")' >"$WORK/scripts/gate.sh"
printf '%s\n' 'd["tool_input"]["command"]' >"$WORK/.githooks/hooks.d/x"
printf '%s\n' "jq '.tool_input[\"command\"]'" >"$WORK/hooks/pre.sh"
printf '%s\n' "jq -r '.tool_input | .command'" >"$WORK/hooks/piped.sh"
printf '%s\n' 'ti = d["tool_input"]' 'cmd = ti.get("command")' >"$WORK/scripts/split.py.sh"
printf '%s\n' 'jq -r .tool_input.command' >"$WORK/scripts/lint-locked"
printf '%s\n' "jq -r '.tool_input.command'" >"$WORK/scripts/sub/nested.sh"
printf '%s\n' "jq -r '.tool_input | .commands'" >"$WORK/scripts/plural.sh"
printf '%s\n' 'jq -r .tool_input.command' >"$WORK/scripts/gate_test.sh"
printf '%s\n' '{"hooks": [{"command": "bash x.sh"}]}' >"$WORK/hooks/hooks.json"
got="$(readers "$WORK" | sort | tr '\n' ' ')"
want=".githooks/hooks.d/x hooks/piped.sh hooks/pre.sh scripts/gate.sh scripts/lint-locked scripts/split.py.sh scripts/sub/nested.sh "
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
