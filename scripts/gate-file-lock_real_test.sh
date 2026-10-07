#!/usr/bin/env bash
# gate-file-lock.sh against the REAL loto binary, two sessions in one repo
# (loto-wuzh). gate-file-lock_test.sh drives a stub; a stub that prints what
# the gate expects instead of what loto prints is how a root session's write
# over a peer's live lock passed 114/114 while the gate let it through. This
# suite keeps the gate honest to the binary's actual output.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOOK="$ROOT/scripts/gate-file-lock.sh"
WORK="$(mktemp -d)"
sleep 600 &
HOLDER_PID=$!
trap 'kill "$HOLDER_PID" 2>/dev/null; rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
(cd "$ROOT" && go build -o "$WORK/bin/loto" ./cmd/loto) || { echo "✗ build failed"; exit 1; }
export PATH="$WORK/bin:$PATH" LOTO_BASE="$WORK/base" TMPDIR="$WORK"
unset LOTO_AGENT_ID LOTO_SUBAGENT_ID

REPO="$WORK/repo"
mkdir -p "$REPO/pkg"
cd "$REPO" || exit 1
git init -q && git remote add origin https://github.com/example/gate-real.git
echo a >a.go && echo b >pkg/b.go && git add . &&
  git -c user.email=t@t -c user.name=t -c core.hooksPath=/dev/null commit -qm "test: fixture"

# Both sessions are live: their whoami records name a running pid, so a lock
# reads liveness=alive and plain `loto check` blocks on it.
as() { local sid="$1"; shift; CLAUDE_CODE_SESSION_ID="$sid" LOTO_PID="$HOLDER_PID" "$@"; }
as sessA loto whoami >/dev/null
as sessG loto whoami >/dev/null
as sessA loto lock a.go -t "hold" >/dev/null
as sessA loto claim pkg -t "claim pkg" >/dev/null

pass=0 fail=0
# run NAME WANT_RC SID PAYLOAD [STDERR_SUBSTRING]
run() {
  local name="$1" want="$2" sid="$3" payload="$4" needle="${5:-}" got err
  rm -rf "$WORK/loto-check-cache"
  err="$(printf '%s' "$payload" | as "$sid" bash "$HOOK" 2>&1 >/dev/null)"
  got=$?
  if [ "$got" = "$want" ] && { [ -z "$needle" ] || [[ "$err" == *"$needle"* ]]; }; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want rc=%s got rc=%s\n%s\n' "$name" "$want" "$got" "$err"
  fi
}
edit() { printf '{"session_id":"%s","tool_name":"Edit","cwd":"%s","tool_input":{"file_path":"%s/%s"}}' "$1" "$REPO" "$REPO" "$2"; }
sub_edit() { printf '{"session_id":"%s","agent_id":"%s","tool_name":"Edit","cwd":"%s","tool_input":{"file_path":"%s/%s"}}' "$1" "$2" "$REPO" "$REPO" "$3"; }
bash_cmd() { printf '{"session_id":"%s","tool_name":"Bash","cwd":"%s","tool_input":{"command":"%s"}}' "$1" "$REPO" "$2"; }

# --- a peer's live lock blocks every write shape ---------------------------
run "root peer's Edit over a live lock is refused, naming the holder" 2 sessG "$(edit sessG a.go)" "blocker=sessA"
run "root peer's Bash redirect over a live lock is refused" 2 sessG "$(bash_cmd sessG 'echo hi > a.go')" "blocker=sessA"
run "subagent peer's Edit over a live lock is refused" 2 sessG "$(sub_edit sessG sib1 a.go)" "blocker=sessA"
run "the holder's own Edit is allowed" 0 sessA "$(edit sessA a.go)"

# --- a claim never out-ranks the caller's own lock (sd-cpbj) ---------------
# Only check --gate (a subagent) denies on a claim; a root session's plain
# check reports it as advisory and allows.
run "subagent with no lock under a peer's claim is refused" 2 sessG "$(sub_edit sessG sib1 pkg/b.go)"
as sessG loto lock pkg/b.go -t "mine" >/dev/null
run "subagent whose session holds the file is allowed under a peer's claim" 0 sessG "$(sub_edit sessG sib1 pkg/b.go)"
run "root session holding the file is allowed under a peer's claim" 0 sessG "$(edit sessG pkg/b.go)"

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
