#!/usr/bin/env bash
# loto SessionEnd cleanup. Releases every lock held by this session's agent
# identity — but only when that scope carries a single intent.
#
# N concurrent Claude Code lanes can share one owner id, and one session id
# too, so this hook firing at one lane's end cannot tell "everything I own"
# from "everything my fleet owns" by owner/session alone (sd-xhap). More than
# one intent among `loto status --mine`'s rows is the only signal available
# that a peer lane's locks are mixed in here — and unlike an interactive
# `unlock --all`, nobody reads this hook's output before it acts, so the
# multi-intent case refuses (loto's Rules allow either; a headless call site
# is exactly the case with no human to read a warning first). A single-intent
# scope (the common case: one lane, or a shared owner id with nothing else in
# flight) still gets the full sweep, unchanged from before.
#
# Safe to no-op if no locks are held, intents disagree, `loto status` cannot
# be read, or loto isn't installed.

set -u

if ! command -v loto >/dev/null 2>&1; then
  exit 0
fi

# Could-not-look and owns-nothing are different states and must not share a
# branch: `loto status --mine` exits 3 with empty stdout outside a loto
# project (measured 2026-09-08), and a pipeline would turn that into "0
# intents" — the unconditional sweep this hook exists to prevent. Capture the
# output and the exit status apart, and refuse on failure.
if ! mine=$(loto status --mine 2>/dev/null); then
  exit 0
fi

intents=$(printf '%s\n' "$mine" | grep -o 'intent="[^"]*"' | sort -u | wc -l | tr -d ' ')

# 0 or 1 distinct intent: nothing owned, or everything owned belongs to one
# lane's own task — a full sweep cannot drop a peer's lock either way.
if [ "${intents:-0}" -le 1 ]; then
  loto unlock --all -t "session end" >/dev/null 2>&1 || true
fi
exit 0
