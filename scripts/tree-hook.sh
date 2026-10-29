#!/usr/bin/env bash
# tree-hook.sh <pre|post> — run `loto hook <verb>` for the plugin's tree-state
# hooks (stdin is the harness event, passed through).
#
# ‡ Fail open when loto is missing or predates the `hook` verb. A bare
# `loto hook pre` against a stale binary exits 2 ("unknown command"), and exit 2
# in a PreToolUse hook blocks the tool call — Bash included, so the session
# could not run `make install` to repair it. A real refusal from a current
# loto (exit 2 with the holder named) still passes through unchanged.
verb=${1:-}
case "$verb" in pre | post) ;; *) exit 0 ;; esac
command -v loto >/dev/null 2>&1 || exit 0
loto hook --help >/dev/null 2>&1 || exit 0
exec loto hook "$verb"
