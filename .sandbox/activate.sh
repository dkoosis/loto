#!/usr/bin/env bash
# Source this to activate Go dev environment for loto.
# Usage: source .sandbox/activate.sh
# Resolves own path — works from any working directory, bash or zsh.

if [ -n "${ZSH_VERSION:-}" ]; then
  eval '_SANDBOX_SRC="${(%):-%x}"'
else
  _SANDBOX_SRC="${BASH_SOURCE[0]}"
fi
_SANDBOX_DIR="$(cd "$(dirname "$_SANDBOX_SRC")" && pwd)"
unset _SANDBOX_SRC

# shellcheck source=/dev/null
source "$_SANDBOX_DIR/lib/lib-activate.sh"

unset _SANDBOX_DIR
