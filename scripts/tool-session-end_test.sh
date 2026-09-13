#!/usr/bin/env bash
# Tests for tool-session-end.sh (sd-xhap): the SessionEnd hook must sweep
# `unlock --all` when its own scope carries one intent (or none), and refuse
# the sweep — rather than guess — when `loto status --mine` shows more than
# one, the signal that a peer lane's locks share this owner id.
#
# It must also refuse when `loto status --mine` FAILS: could-not-look is not
# owns-nothing, and the real CLI exits 3 with empty stdout outside a loto
# project.
#
# Stubs `loto` on PATH: `status --mine` prints the intent lines $STATUS_MINE
# supplies (already in the real CLI's `intent="..."` shape) and exits
# $STATUS_RC; `unlock --all` just records that it ran, so a test can assert
# whether the sweep fired without a real store.
set -u

HOOK="$(cd "$(dirname "$0")" && pwd)/tool-session-end.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
cat >"$WORK/bin/loto" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  status)
    printf '%s\n' "${STATUS_MINE:-}"
    exit "${STATUS_RC:-0}"
    ;;
  unlock)
    echo "$*" >>"${UNLOCKLOG:-/dev/null}"
    exit 0
    ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$WORK/bin/loto"
export PATH="$WORK/bin:$PATH"

pass=0 fail=0

# run NAME STATUS_MINE_FIXTURE WANT_SWEPT(0|1) [STATUS_EXIT_CODE]
run() {
  local name="$1" fixture="$2" want="$3" got
  export STATUS_MINE="$fixture"
  export STATUS_RC="${4:-0}"
  export UNLOCKLOG="$WORK/unlocklog"; : >"$UNLOCKLOG"
  bash "$HOOK" >/dev/null 2>&1
  if [ -s "$UNLOCKLOG" ]; then got=1; else got=0; fi
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want swept=%s got swept=%s\n' "$name" "$want" "$got"
  fi
}

one_lane_two_locks=$(cat <<'EOF'
✓ target=a.go owner=x epoch=1 mode=exclusive intent="sd-xhap: work" held_since=t ttl_remaining=1 liveness=alive host=h pid=0 branch=b
✓ target=b.go owner=x epoch=1 mode=exclusive intent="sd-xhap: work" held_since=t ttl_remaining=1 liveness=alive host=h pid=0 branch=b
EOF
)
two_lanes=$(cat <<'EOF'
✓ target=a.go owner=x epoch=1 mode=exclusive intent="laneA: work" held_since=t ttl_remaining=1 liveness=alive host=h pid=0 branch=b
✓ target=c.go owner=x epoch=1 mode=exclusive intent="laneB: work" held_since=t ttl_remaining=1 liveness=alive host=h pid=0 branch=b
EOF
)

run "no locks owned: sweep is a harmless no-op call" "ℹ no locks owned" 1
run "one lane, several locks, one intent: sweeps" "$one_lane_two_locks" 1
run "two lanes' intents present: refuses the sweep" "$two_lanes" 0

# `loto status --mine` failing is not evidence that nothing is held. Outside a
# loto project the real CLI exits 3 with empty stdout; a store error is the
# same shape with locks actually out there. Both must refuse.
run "status exits nonzero, empty output: refuses the sweep" "" 0 3
run "status exits nonzero with rows: refuses the sweep" "$one_lane_two_locks" 0 3
export STATUS_RC=0

# loto missing from PATH: no-op, never errors, never sweeps.
export PATH="$WORK/bin-empty:$PATH"
mkdir -p "$WORK/bin-empty"
save_path="$PATH"
export PATH="/usr/bin:/bin"
export UNLOCKLOG="$WORK/unlocklog"; : >"$UNLOCKLOG"
bash "$HOOK" >/dev/null 2>&1
rc=$?
export PATH="$save_path"
if [ "$rc" = 0 ] && [ ! -s "$UNLOCKLOG" ]; then
  pass=$((pass + 1)); printf '✓ %s\n' "loto not installed: exits 0, never sweeps"
else
  fail=$((fail + 1)); printf '✗ %s — rc=%s log=%s\n' "loto not installed: exits 0, never sweeps" "$rc" "$(cat "$UNLOCKLOG" 2>/dev/null)"
fi

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
