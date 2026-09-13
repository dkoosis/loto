#!/usr/bin/env bash
# Tests for gate-file-lock.sh. Stubs `loto` on PATH so we can drive the gate's
# decision without real cross-session locks. The stub treats any path whose
# string contains a token listed in $LOCKED (newline-separated) as peer-held
# (exit 1); everything else is unlocked (exit 0). It records every checked path
# to $CHECKLOG so we can assert which paths the gate inspected.
set -u

HOOK="$(cd "$(dirname "$0")" && pwd)/gate-file-lock.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT


# --- loto stub ---------------------------------------------------------------
mkdir -p "$WORK/bin"
cat >"$WORK/bin/loto" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  check)
    shift
    flags=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --gate) flags="${flags:+$flags,}gate"; shift ;;
        -staged|--staged) exit 0 ;;
        *) break ;;
      esac
    done
    p="${1:-}"
    # Two columns so a test can assert WHICH flags a call sent, not just that
    # loto was consulted.
    # Third column: WHO checked (LOTO_SUBAGENT_ID), so a test can assert the
    # check carries the same stamp as the beacon (loto-wofb).
    if [ -n "${LOTO_SUBAGENT_ID:-}" ]; then
      printf '%s\t%s\t%s\n' "$flags" "$p" "$LOTO_SUBAGENT_ID" >>"${CHECKLOG:-/dev/null}"
    else
      printf '%s\t%s\n' "$flags" "$p" >>"${CHECKLOG:-/dev/null}"
    fi
    while IFS= read -r tok; do
      [ -n "$tok" ] || continue
      case "$p" in *"$tok"*) echo "✗ blocked $p held by peer"; exit 1 ;; esac
    done <<<"${LOCKED:-}"
    exit 0
    ;;
  beacon)
    shift
    # Records WHO minted (LOTO_SUBAGENT_ID) and for WHAT, so a test can assert
    # the per-sibling stamp actually reaches the binary (loto-kjqp).
    printf '%s\t%s\n' "${LOTO_SUBAGENT_ID:-}" "${1:-}" >>"${BEACONLOG:-/dev/null}"
    exit 0
    ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$WORK/bin/loto"
export PATH="$WORK/bin:$PATH"

# --- fixture tree ------------------------------------------------------------
# The gate only inspects a token that resolves to something real: the file
# itself, or the directory a not-yet-created file would land in (sd-8a6w).
# Every path this suite drives through the gate is fictional, so build the
# directories they name and run from there — without this the suite would be
# asserting against tokens the gate correctly drops as prose, which is the
# false-clean shape .claude/rules/standard-checks.md exists to stop.
mkdir -p "$WORK/repo/internal/score/deep" "$WORK/repo/internal/other" "$WORK/repo/sub"
# The absolute fixtures are literals in the assertions below, so their parents
# have to exist where they are spelled. Created empty and removed on exit.
_abs_fixtures="/tmp/loto-beacon /tmp/loto-wofb /tmp/nvhp /tmp/stash /tmp/s"
mkdir -p $_abs_fixtures
trap 'rm -rf "$WORK"; rmdir $_abs_fixtures 2>/dev/null' EXIT
cd "$WORK/repo" || exit 1

# Pin the hook's clean-verdict cache inside $WORK. Without this it lands in the
# shared system temp dir, and a 5s-old verdict left by a previous suite run
# leaks in — so the result would depend on how recently the suite last ran.
export TMPDIR="$WORK"

pass=0 fail=0
# run NAME EXPECT_RC ENVELOPE  (LOCKED taken from env)
run() {
  local name="$1" want="$2" env="$3"
  export CHECKLOG="$WORK/checklog"
  : >"$CHECKLOG"
  printf '%s' "$env" | bash "$HOOK" >/dev/null 2>&1
  local got=$?
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want rc=%s got rc=%s\n' "$name" "$want" "$got"
  fi
}

bash_env()  { printf '{"tool_name":"Bash","tool_input":{"command":%s}}' "$(jq -Rn --arg c "$1" '$c')"; }
edit_env()  { printf '{"tool_name":"Edit","tool_input":{"file_path":%s}}' "$(jq -Rn --arg p "$1" '$p')"; }
sub_edit_env() { printf '{"tool_name":"Edit","agent_id":%s,"tool_input":{"file_path":%s}}' "$(jq -Rn --arg a "$1" '$a')" "$(jq -Rn --arg p "$2" '$p')"; }
sub_bash_env() { printf '{"tool_name":"Bash","agent_id":%s,"tool_input":{"command":%s}}' "$(jq -Rn --arg a "$1" '$a')" "$(jq -Rn --arg c "$2" '$c')"; }

# --- Bash gate: blocks ---
export LOCKED="internal/score/landmark.go"
run "mv of peer-locked file blocks" 2 "$(bash_env 'mv internal/score/landmark.go /tmp/stash/landmark.go')"
run "rm of peer-locked file blocks" 2 "$(bash_env 'rm -rf internal/score/landmark.go')"
run "cp onto peer-locked dest blocks" 2 "$(bash_env 'cp /tmp/new.go internal/score/landmark.go')"
run "git mv of peer-locked file blocks" 2 "$(bash_env 'git mv internal/score/landmark.go x.go')"
run "compound (&&) still scans mv" 2 "$(bash_env 'mkdir -p /tmp/s && mv internal/score/landmark.go /tmp/s/')"

# --- Bash gate: allows ---
run "mv of unlocked file allows" 0 "$(bash_env 'mv internal/other/foo.go /tmp/x.go')"
run "non-destructive verb allows + no check" 0 "$(bash_env 'go test ./internal/score/...')"
run "cat of locked file allows (read-only)" 0 "$(bash_env 'cat internal/score/landmark.go')"
# verify the read-only case never consulted loto
export CHECKLOG="$WORK/checklog2"; : >"$CHECKLOG"
printf '%s' "$(bash_env 'cat internal/score/landmark.go')" | bash "$HOOK" >/dev/null 2>&1
if [ -s "$CHECKLOG" ]; then fail=$((fail+1)); echo "✗ cat must not invoke loto check"; else pass=$((pass+1)); echo "✓ cat must not invoke loto check"; fi

# --- Edit gate: regression (unchanged behavior) ---
run "Edit of peer-locked file blocks" 2 "$(edit_env 'internal/score/landmark.go')"
run "Edit of unlocked file allows" 0 "$(edit_env 'internal/other/foo.go')"

# --- plumbing: no LOCKED → nothing blocks ---
export LOCKED=""
run "no locks → mv allows" 0 "$(bash_env 'mv internal/score/landmark.go /tmp/x.go')"

# --- beacon minting (loto-kjqp / loto-xwod) ---------------------------------
# beacon_run NAME EXPECT_MINT ENVELOPE — EXPECT_MINT is the stamp\tpath line the
# stub should have recorded, or "" for "nothing minted".
beacon_run() {
  local name="$1" want="$2" env="$3" got
  export BEACONLOG="$WORK/beaconlog"; : >"$BEACONLOG"
  export CHECKLOG="$WORK/checklog-b"; : >"$CHECKLOG"
  printf '%s' "$env" | bash "$HOOK" >/dev/null 2>&1
  got="$(cat "$BEACONLOG" 2>/dev/null)"
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want %q got %q\n' "$name" "$want" "$got"
  fi
}

export LOCKED=""
beacon_run "subagent Edit mints a beacon stamped with its agent_id" \
  "$(printf 'sib-a\t/tmp/loto-beacon/a.go')" "$(sub_edit_env 'sib-a' '/tmp/loto-beacon/a.go')"
beacon_run "a session with no agent_id mints nothing" \
  "" "$(edit_env '/tmp/loto-beacon/a.go')"
beacon_run "subagent Bash mints for the destructive verb's operand" \
  "$(printf 'sib-b\t/tmp/loto-beacon/b.go')" "$(sub_bash_env 'sib-b' 'rm /tmp/loto-beacon/b.go')"

# A blocked write must NOT announce itself — the beacon says "I am writing
# here", and the whole point of the block is that it is not.
export LOCKED="/tmp/loto-beacon/held.go"
beacon_run "a blocked subagent write mints nothing" \
  "" "$(sub_edit_env 'sib-c' '/tmp/loto-beacon/held.go')"
export LOCKED=""

# --- stamped + gated check (loto-wofb) ---------------------------------------
# check_run NAME EXPECT_LINE ENVELOPE — EXPECT_LINE is the flags\tpath\tstamp
# line the stub must have recorded for the check.
check_run() {
  local name="$1" want="$2" env="$3" got
  export CHECKLOG="$WORK/checklog-w"; : >"$CHECKLOG"
  printf '%s' "$env" | bash "$HOOK" >/dev/null 2>&1
  got="$(cat "$CHECKLOG" 2>/dev/null)"
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want %q got %q\n' "$name" "$want" "$got"
  fi
}
cache_clear() { rm -rf "$WORK/loto-check-cache"; }
export LOCKED=""
cache_clear
check_run "subagent Edit checks under its own stamp with --gate" \
  "$(printf 'gate\t/tmp/loto-wofb/a.go\tsib-w1')" "$(sub_edit_env 'sib-w1' '/tmp/loto-wofb/a.go')"
check_run "a session with no agent_id runs a plain, unstamped check" \
  "$(printf '\t/tmp/loto-wofb/b.go')" "$(edit_env '/tmp/loto-wofb/b.go')"
# The cache is keyed per sibling: w1's clean verdict must not be served to w2.
check_run "a second sibling is not served the first one's cached verdict" \
  "$(printf 'gate\t/tmp/loto-wofb/a.go\tsib-w2')" "$(sub_edit_env 'sib-w2' '/tmp/loto-wofb/a.go')"
cache_clear

# --- fail-open notices + contract stamp (loto-tzmv.7) -----------------------
# stderr_run NAME EXPECT_SUBSTR ENVELOPE [PATH_OVERRIDE] — asserts the hook says
# something on stderr, since a silent fail-open is the failure this closes.
# NOLOTO is a PATH with the system tools the hook needs but no `loto` — the
# stub lives in $WORK/bin, which this deliberately omits. Invoke through an
# absolute bash, since the modified PATH is what would otherwise resolve it.
NOLOTO="/usr/bin:/bin"
stderr_run() {
  local name="$1" want="$2" env="$3" path_override="${4:-$PATH}" got
  export CHECKLOG="$WORK/checklog-s"; : >"$CHECKLOG"
  got="$(printf '%s' "$env" | PATH="$path_override" /bin/bash "$HOOK" 2>&1 >/dev/null)"
  if [[ "$got" == *"$want"* ]]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — stderr %q lacks %q\n' "$name" "$got" "$want"
  fi
}

export LOCKED=""
sess_edit_env() { printf '{"tool_name":"Edit","session_id":%s,"tool_input":{"file_path":%s}}' "$(jq -Rn --arg s "$1" '$s')" "$(jq -Rn --arg p "$2" '$p')"; }

# A loto that is not on PATH is the 2026-08-12 incident class: the gate runs,
# answers nothing, and the fleet believes it is covered.
stderr_run "missing loto announces the fail-open" "loto=not-on-PATH" \
  "$(sess_edit_env 'sess-nl' '/tmp/x.go')" "$NOLOTO"

# Once per session — the gate fires on every tool call, and a row repeated
# hundreds of times is one the model learns to skip.
export CHECKLOG="$WORK/checklog-s2"
second="$(printf '%s' "$(sess_edit_env 'sess-nl' '/tmp/x.go')" | PATH="$NOLOTO" /bin/bash "$HOOK" 2>&1 >/dev/null)"
if [ -z "$second" ]; then
  pass=$((pass+1)); echo "✓ the missing-loto notice fires once per session"
else
  fail=$((fail+1)); printf '✗ the missing-loto notice fires once per session — got %q\n' "$second"
fi

# A different session has not been told yet.
stderr_run "a second session is told too" "loto=not-on-PATH" \
  "$(sess_edit_env 'sess-other' '/tmp/x.go')" "$NOLOTO"

# The contract stamp must reach the binary, or loto cannot tell it is stale.
cat >"$WORK/bin/loto-contract-probe" <<'PROBE'
#!/usr/bin/env bash
printf '%s\n' "${LOTO_GATE_CONTRACT:-unset}" >>"${CONTRACTLOG:-/dev/null}"
exit 0
PROBE
chmod +x "$WORK/bin/loto-contract-probe"
export CONTRACTLOG="$WORK/contractlog"; : >"$CONTRACTLOG"
mkdir -p "$WORK/bin2"
cp "$WORK/bin/loto-contract-probe" "$WORK/bin2/loto"
printf '%s' "$(edit_env '/tmp/contract.go')" | PATH="$WORK/bin2:$PATH" bash "$HOOK" >/dev/null 2>&1
if grep -qx '[0-9][0-9]*' "$CONTRACTLOG" 2>/dev/null; then
  pass=$((pass+1)); echo "✓ LOTO_GATE_CONTRACT reaches the binary"
else
  fail=$((fail+1)); printf '✗ LOTO_GATE_CONTRACT reaches the binary — got %q\n' "$(cat "$CONTRACTLOG")"
fi

# --- shell write extraction (loto-nvhp) --------------------------------------
# The four shapes the gate claims: sed -i, > / >>, tee, cp/mv/rm operands. Each
# gets a block case and an allow case; the shapes we knowingly DO NOT catch are
# pinned too, because an unpinned hole is one a later refactor "fixes" by
# accident and nobody notices when it reopens.

# The clean-verdict cache lives 5s keyed by path, so a path a previous case saw
# clean would be answered from cache here instead of from $LOCKED. Every case
# below drives its own verdict, so each starts from an empty cache.
cache_clear() { rm -rf "$WORK/loto-check-cache" 2>/dev/null; }
runc() { cache_clear; run "$@"; }

# checked NAME EXPECT ENVELOPE — EXPECT is the exact flags\tpath log the stub
# should hold, or "" for "loto was never consulted". Asserting WHICH path was
# checked is the only way to catch a wrong base: checking the wrong path
# answers "no conflicts" and reads exactly like a pass.
checked() {
  local name="$1" want="$2" env="$3" got
  cache_clear
  export CHECKLOG="$WORK/checklog-w"; : >"$CHECKLOG"
  printf '%s' "$env" | bash "$HOOK" >/dev/null 2>&1
  got="$(cat "$CHECKLOG" 2>/dev/null)"
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1)); printf '✓ %s\n' "$name"
  else
    fail=$((fail + 1)); printf '✗ %s — want %q got %q\n' "$name" "$want" "$got"
  fi
}

export LOCKED="internal/score/landmark.go"

# sed -i ---------------------------------------------------------------------
runc "sed -i (GNU form) blocks" 2 "$(bash_env "sed -i 's/a/b/' internal/score/landmark.go")"
runc "sed -i '' (BSD form) blocks" 2 "$(bash_env "sed -i '' 's/a/b/' internal/score/landmark.go")"
runc "sed -i.bak -e blocks" 2 "$(bash_env "sed -i.bak -e 's/a/b/' internal/score/landmark.go")"
runc "sed --in-place blocks" 2 "$(bash_env "sed --in-place 's/a/b/' internal/score/landmark.go")"
runc "sed without -i allows" 0 "$(bash_env "sed 's/a/b/' internal/score/landmark.go")"
checked "sed without -i never consults loto" "" \
  "$(bash_env "sed -n '1,5p' internal/score/landmark.go")"
# The script argument is not a filename — checking it would be a wasted round
# trip over an expression that names no file.
checked "the sed script arg is not mistaken for a file" "$(printf '\t/tmp/nvhp/x.go')" \
  "$(bash_env "sed -i 's/a/b/' /tmp/nvhp/x.go")"
checked "the BSD backup suffix is not mistaken for a file" "$(printf '\t/tmp/nvhp/x.go')" \
  "$(bash_env "sed -i '' 's/a/b/' /tmp/nvhp/x.go")"

# > and >> -------------------------------------------------------------------
runc "redirect > onto a peer-locked file blocks" 2 "$(bash_env 'echo hi > internal/score/landmark.go')"
runc "redirect >> onto a peer-locked file blocks" 2 "$(bash_env 'echo hi >> internal/score/landmark.go')"
runc "glued redirect >file blocks" 2 "$(bash_env 'echo hi >internal/score/landmark.go')"
runc "fd-qualified redirect 2>file blocks" 2 "$(bash_env 'go build ./... 2> internal/score/landmark.go')"
runc "redirect onto an unlocked file allows" 0 "$(bash_env 'echo hi > internal/other/foo.go')"
# A redirect writes whatever the verb is, so the verb must not gate the pass:
# `cat locked` is a read, `cat x > locked` is not.
runc "a read verb with a write redirect still blocks" 2 "$(bash_env 'cat /tmp/x > internal/score/landmark.go')"
# Latency: >/dev/null is in half the commands a session runs, and every
# consulted path costs a loto round trip (~0.2s idle, seconds under fleet
# contention).
checked "redirect to /dev/null never consults loto" "" "$(bash_env 'go build ./... > /dev/null 2>&1')"
# `>` that is not a redirect operator: a loose match here would name a path the
# caller never wrote.
checked "an arrow inside a word is not a redirect" "" "$(bash_env 'echo a->b')"
checked "stderr dup 2>&1 is not a write" "" "$(bash_env 'go build ./... 2>&1')"

# tee ------------------------------------------------------------------------
runc "tee onto a peer-locked file blocks" 2 "$(bash_env 'echo hi | tee internal/score/landmark.go')"
runc "tee -a onto a peer-locked file blocks" 2 "$(bash_env 'echo hi | tee -a internal/score/landmark.go')"
runc "tee onto an unlocked file allows" 0 "$(bash_env 'echo hi | tee internal/other/foo.go')"

# cp/mv destination ----------------------------------------------------------
runc "cp destination blocks" 2 "$(bash_env 'cp /tmp/new.go internal/score/landmark.go')"
runc "mv destination blocks" 2 "$(bash_env 'mv /tmp/new.go internal/score/landmark.go')"

# leading cd -----------------------------------------------------------------
# docs/DESIGN.md: a Bash command's base is the payload cwd PLUS any leading cd
# in the same command. Without that, `cd internal/score && rm landmark.go`
# checks a bare `landmark.go` against the payload cwd — a different file, and a
# clean verdict for a path nobody resolved.
runc "cd re-roots a relative operand (blocks)" 2 "$(bash_env 'cd internal/score && rm landmark.go')"
checked "cd re-roots to the path actually written" "$(printf '\tinternal/other/landmark.go')" \
  "$(bash_env 'cd internal/other && rm landmark.go')"
checked "a nested cd composes" "$(printf '\tinternal/score/deep/landmark.go')" \
  "$(bash_env 'cd internal/score && cd deep && rm landmark.go')"
checked "a subshell cd does not leak past its )" "$(printf '\tinternal/score/a.go\n\tsub/b.go')" \
  "$(bash_env '(cd internal/score && rm a.go) && rm sub/b.go')"
# An unreadable cd target: the Bash scan has always failed OPEN, so it stays
# open here rather than growing a refusal that would block `cd "$d" && rm x`.
# It is not silent about it — a silent fail-open is the thing this file exists
# to avoid.
runc "an unreadable cd target fails open, not closed" 0 "$(bash_env 'cd "$d" && rm landmark.go')"
stderr_run "an unreadable cd target says so" "cd'd somewhere" "$(bash_env 'cd "$d" && rm landmark.go')"
# heredoc --------------------------------------------------------------------
# The body is data. `cat > f <<EOF` has its redirect gated on the first line;
# scanning the body would block on every path the FILE MENTIONS.
runc "a heredoc redirect onto a peer-locked file blocks" 2 \
  "$(bash_env "$(printf 'cat > internal/score/landmark.go <<EOF\nhello\nEOF')")"
checked "a heredoc body is not scanned for writes" "$(printf '\tinternal/other/foo.go')" \
  "$(bash_env "$(printf "cat > internal/other/foo.go <<'EOF'\nrm internal/score/landmark.go\nsed -i 's/a/b/' internal/score/landmark.go\nEOF")")"

# --- script-driven writes: a heredoc fed to an interpreter (sd-wgcw) --------
# The repro: hold a lock on a path under another identity, run a python
# heredoc that writes it — the gate must now catch it instead of waving it
# through as data.
runc "python heredoc: open(path,'w') blocks" 2 \
  "$(bash_env "$(printf "python3 <<'EOF'\nopen('internal/score/landmark.go', 'w').write('x')\nEOF")")"
runc "python heredoc: os.remove(path) blocks" 2 \
  "$(bash_env "$(printf "python3 <<'EOF'\nimport os\nos.remove('internal/score/landmark.go')\nEOF")")"
runc "python heredoc: open(path,'r') (a read) allows" 0 \
  "$(bash_env "$(printf "python3 <<'EOF'\nd = open('internal/score/landmark.go', 'r').read()\nEOF")")"
runc "python heredoc onto an unlocked path allows" 0 \
  "$(bash_env "$(printf "python3 <<'EOF'\nopen('internal/other/foo.go', 'w').write('x')\nEOF")")"
runc "ruby heredoc: File.write(path,...) blocks" 2 \
  "$(bash_env "$(printf "ruby <<'EOF'\nFile.write('internal/score/landmark.go', 'x')\nEOF")")"
runc "node heredoc: fs.writeFileSync(path,...) blocks" 2 \
  "$(bash_env "$(printf "node <<'EOF'\nfs.writeFileSync('internal/score/landmark.go', 'x')\nEOF")")"
runc "perl heredoc: 3-arg open(\$fh,'>',path) blocks" 2 \
  "$(bash_env "$(printf "perl <<'EOF'\nopen(my \\\$fh, '>', 'internal/score/landmark.go');\nEOF")")"
runc "bash heredoc: nested rm blocks (the body is itself shell)" 2 \
  "$(bash_env "$(printf "bash <<'EOF'\nrm internal/score/landmark.go\nEOF")")"
runc "sh heredoc: nested redirect blocks" 2 \
  "$(bash_env "$(printf "sh <<'EOF'\necho hi > internal/score/landmark.go\nEOF")")"
# An inner heredoc inside a scanned bash body ends the delimiter we were
# counting to, but the body is itself shell, so the ordinary scan keeps
# reading and the outer write is still caught (probed 2026-08-28).
runc "bash heredoc: a nested inner heredoc does not lose the outer write" 2 \
  "$(bash_env "$(printf "bash <<'OUTER'\ncat > /tmp/z <<'INNER'\nhello\nINNER\nrm internal/score/landmark.go\nOUTER")")"
# A heredoc fed to an interpreter this hook does not recognize is still data —
# same contract as `cat`, just a different unrecognized verb.
runc "heredoc fed to an unrecognized verb stays data" 0 \
  "$(bash_env "$(printf "unknown-tool <<'EOF'\nrm internal/score/landmark.go\nEOF")")"

# --- install / dd (sd-wgcw) --------------------------------------------------
runc "install destination blocks" 2 \
  "$(bash_env 'install /tmp/new.go internal/score/landmark.go')"
runc "install onto an unlocked destination allows" 0 \
  "$(bash_env 'install /tmp/new.go internal/other/foo.go')"
runc "dd of= onto a peer-locked file blocks" 2 \
  "$(bash_env 'dd if=/tmp/new.go of=internal/score/landmark.go')"
runc "dd if= of a peer-locked file (a read) allows" 0 \
  "$(bash_env 'dd if=internal/score/landmark.go of=/tmp/copy.go')"

# --- the holes we are keeping, pinned ---------------------------------------
# Each of these WRITES a peer-locked file and is allowed through. They are
# misses, not false cleans: nothing here reports "no conflicts" for a path it
# failed to resolve — the gate simply never saw the write. Pinned so a later
# change that closes one is a deliberate change with a failing test to update.
runc "MISS: a python -c one-liner writing the file (not a heredoc)" 0 \
  "$(bash_env 'python3 -c "open(\"internal/score/landmark.go\",\"w\").write(\"x\")"')"
# Probed 2026-08-28 while reviewing sd-wgcw; each is named in the hook header.
runc "MISS: a two-path call whose DEST is the second operand (src is read instead)" 0 \
  "$(bash_env "$(printf "python3 <<'EOF'\nimport shutil\nshutil.copy('/tmp/a', 'internal/score/landmark.go')\nEOF")")"
runc "MISS: an interpreter behind a wrapper — the lang is read off the first verb" 0 \
  "$(bash_env "$(printf "env python3 <<'EOF'\nopen('internal/score/landmark.go','w')\nEOF")")"
runc "MISS: a make target writing the file" 0 "$(bash_env 'make generate')"
stderr_run "make announces itself as unparsed" "NOT checked" "$(bash_env 'make generate')"
runc "MISS: sed -i with a | delimiter (the splitter cuts the command)" 0 \
  "$(bash_env "sed -i 's|a|b|' internal/score/landmark.go")"
runc "MISS: a redirect target we cannot expand" 0 "$(bash_env 'echo hi > $OUT')"
runc "MISS: a glob destination" 0 "$(bash_env 'cp /tmp/new.go internal/score/*.go')"
checked "an unexpandable target is skipped, never checked as a literal" "" \
  "$(bash_env 'echo hi > "$OUT"')"
# git checkout/restore/apply certainly write, and we do not parse their
# operands — so the miss is announced rather than swallowed.
runc "MISS: git checkout of the file" 0 "$(bash_env 'git checkout internal/score/landmark.go')"
stderr_run "an unparsed writer announces itself" "NOT checked" \
  "$(bash_env 'git checkout internal/score/landmark.go')"
stderr_run "patch announces itself too" "NOT checked" "$(bash_env 'patch -p1 < /tmp/x.diff')"

# beacons on the new shapes --------------------------------------------------
export LOCKED=""
beacon_run "a subagent redirect mints a beacon for its target" \
  "$(printf 'sib-r\t/tmp/loto-beacon/r.go')" "$(sub_bash_env 'sib-r' 'echo hi > /tmp/loto-beacon/r.go')"
cache_clear
beacon_run "a subagent sed -i mints a beacon for its target" \
  "$(printf 'sib-s\t/tmp/loto-beacon/s.go')" "$(sub_bash_env 'sib-s' "sed -i 's/a/b/' /tmp/loto-beacon/s.go")"
cache_clear

# an unexpandable operand must never become a beacon (sd-xtx) ----------------
# `loto doctor` on 2026-08-23 reaped a stale beacon whose target was the token
# `$TMP`. The redirect and cd sites ran path_candidate; the verb-operand loop
# did not, so any token holding a slash skipped the local -e test and went
# straight to check_path and then mint_beacon. The store ended up holding a
# lock on a string that names no file. Each shape below reached the store
# before the fix — a verdict on a path nobody resolved, invariant 9 again.
export LOCKED=""
# The destination is a plain path and SHOULD mint — asserting the exact log
# rather than "empty" is what makes these cases discriminating: a regression
# adds the bad token as a second line instead of silently passing.
for _tok in '$TMP/x' '~/x/y' 'a/*.sh' 'a/?.sh' 'a/[ab].sh'; do
  cache_clear
  beacon_run "no beacon for the unexpandable operand $_tok" \
    "$(printf 'sib-x\t/tmp/loto-beacon/dst.go')" \
    "$(sub_bash_env 'sib-x' "mv $_tok /tmp/loto-beacon/dst.go")"
done
# The guard must not cost a real path its beacon — trading a false lock for a
# missing one is not a fix.
cache_clear
beacon_run "two plain operands both still mint" \
  "$(printf 'sib-x\t/tmp/loto-beacon/plain.go\nsib-x\t/tmp/loto-beacon/dst.go')" \
  "$(sub_bash_env 'sib-x' 'mv /tmp/loto-beacon/plain.go /tmp/loto-beacon/dst.go')"
cache_clear
export LOCKED=""

# --- prose is not a path (sd-8a6w) -------------------------------------------
# The segment splitter cuts a command on ;|& and newlines, so any prose the
# command carries — a commit message, a bead body, a heredoc line — arrives as
# segments, and a segment whose first word happens to be a write verb hands
# this scan its nouns. `the`, `test` and `to` were already dropped for naming
# no file; the old escape hatch was "contains a slash", so the two-word
# fragment `version/loto` was checked, beaconed, and had to be reaped out of
# the store by hand (seen 2026-09-02 in loto, before PR #295).
export LOCKED=""
checked "prose with no write verb consults loto about nothing" "" \
  "$(bash_env 'echo test to version')"
checked "prose under install: a slash is not a path" "" \
  "$(bash_env 'install the test to version/loto')"
checked "prose under rm: same sentence, same silence" "" \
  "$(bash_env 'rm the test to version/loto')"
# ‡ Those three are negative assertions, so on their own they would pass
# hardest if the install/rm scan were dead (standard-checks.md). This one runs the
# same sentence with one resolvable operand in it: the scan IS live here, and
# exactly one token survives the filter.
checked "the one real path in that sentence is still checked" \
  "$(printf '\tinternal/other/foo.go')" \
  "$(bash_env 'install the test to internal/other/foo.go')"

export LOCKED="internal/other/foo.go"
runc "prose naming a peer-held path still blocks" 2 \
  "$(bash_env 'install the test to internal/other/foo.go')"
runc "prose naming nothing real never blocks" 0 \
  "$(bash_env 'install the test to version/loto')"
export LOCKED=""

# The parent-directory allowance is the half that must not be lost: a file the
# command is about to CREATE does not exist yet and still has to be gated.
# Trading a false lock for a missing one is not a fix.
checked "a new file in a real directory is still checked" \
  "$(printf '\tinternal/other/new.go')" "$(bash_env 'echo hi > internal/other/new.go')"
checked "a new file under a directory that does not exist is dropped" "" \
  "$(bash_env 'echo hi > nosuchdir/new.go')"

# --- the Edit path is untouched by the filter (sd-8a6w AC) -------------------
# An Edit/Write names its target explicitly, so it is checked and beaconed
# whether or not the file exists yet; only the shell scan has to guess.
mkdir -p internal/store && : >internal/store/store.go
checked "Edit of a real path is still checked" \
  "$(printf '\tinternal/store/store.go')" "$(edit_env 'internal/store/store.go')"
cache_clear
beacon_run "a subagent Edit of a real path still mints its lock" \
  "$(printf 'sib-ac\tinternal/store/store.go')" \
  "$(sub_edit_env 'sib-ac' 'internal/store/store.go')"
export LOCKED="internal/store/store.go"
runc "Edit of a peer-held real path still blocks" 2 "$(edit_env 'internal/store/store.go')"
export LOCKED=""
cache_clear

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" = 0 ]