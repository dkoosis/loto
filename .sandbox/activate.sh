#!/usr/bin/env bash
# Source this to activate Go dev environment for trixi
# Usage: source .sandbox/activate.sh
# Resolves own path — works from any working directory.

_SANDBOX_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=/dev/null
source "$_SANDBOX_DIR/lib/lib-activate.sh"

unset _SANDBOX_DIR
