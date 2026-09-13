#!/usr/bin/env bash
# loto PreToolUse gate. Runs `loto check <path>` against the file(s) a tool is
# about to touch. Exit 2 (with reason on stderr) blocks the tool call;
# exit 0 lets it proceed.
#
# Two tool shapes are gated:
#   - Edit/Write/MultiEdit/NotebookEdit — the single path in .tool_input.
#   - Bash — writes parsed out of .tool_input.command. This closes the
#     asymmetry where an `Edit` of a peer-held file is refused but a
#     `bash mv peer/file dest` was not (the mechanism behind
#     INCIDENT-ferret-t5d, cross-session variant).
#
# Shapes read out of a shell command (loto-nvhp, extended sd-wgcw):
#   sed -i FILE… · > FILE and >> FILE · tee FILE… · cp/mv/rm (and git mv/rm)
#     operands · install operands · dd of=FILE
#   and, inside a heredoc body fed to an interpreter (python/python3/ruby/
#     node/perl/bash/sh — read off the verb the `<<` follows): the same
#     shell shapes recursively for bash/sh, and a short list of each
#     language's own write calls otherwise (scan_heredoc_line). A heredoc fed
#     to anything else (cat, tee, no recognized interpreter) is still DATA,
#     not commands, and stays unscanned.
# One more is read to keep the others honest: a leading `cd`, which moves
# the base a relative operand resolves against.
#
# ‡ A SECOND shell entry point was gated here — trixi's `agent_shell` MCP tool, which
# ran arbitrary shell and named its command field `.tool_input.command` exactly
# as Bash does, so one scan served both (loto-tzmv.5). That server is retired
# and the tool no longer exists, so the branch went with it (ccp-3cnq).
# What must come back with the next such tool: it keeps its OWN persistent cwd,
# which a hook payload does not carry, so its relative tokens are REFUSED via
# `loto check --cwd-unknown` rather than resolved against a base neither side
# can vouch for (loto-yexz, DESIGN.md invariant 9 — the one place in this scan
# that must not fail open; a false POSITIVE clean is worse than no check).
# The loto binary still ships the flag; the hook half is one revert away —
# `git log -S'--cwd-unknown' -- plugins/loto/scripts/gate-file-lock.sh`.
#
# The Bash scan is a deliberately CONSERVATIVE tripwire that fails OPEN: it acts
# only when it can see a write verb with a literal path operand that loto
# reports peer-held. Shell is not parsed robustly here — that belongs in the
# loto binary (a `loto guard-bash` verb, tracked separately).
#
# ‡ What it MISSES, stated plainly, because a hole you do not name is a hole
# the fleet believes is closed:
#   - a script-driven write outside a heredoc: `python3 -c "open(f,'w')…"`,
#     a `git apply|checkout|restore|stash|clean`, rsync, xargs, editors.
#     `make` and the other verbs that certainly write get one ⚠ per session
#     (see `unparsed`); the rest are silent misses.
#   - inside a scanned heredoc body, a write whose path is not a literal
#     quoted string on the same line — built from a variable, concatenation,
#     or a path stated on an earlier line and reused (`f = open(path); …;
#     f.write(...)` never re-states the path where the write happens) — and
#     Perl's 2-arg `open($fh, "mode+path")` when the mode glyph isn't a plain
#     `>`/`>>`/`<` prefix.
#   - a two-path call whose write target is the SECOND operand:
#     `shutil.copy(src, dst)`, `FileUtils.cp`, `copyFileSync` — extract_quoted
#     reads the FIRST literal, so src is checked and dst is missed. Probed
#     2026-08-28; pinned below.
#   - an interpreter reached through a wrapper: `env python3 <<EOF`,
#     `xargs python3`, `nice ruby`. The lang is read off the FIRST verb of the
#     segment, so a wrapper hides it and the body stays data.
#   - a target the command does not spell literally: `> "$OUT"`, `rm $f`,
#     globs, `~/x`, anything built by a command substitution. path_candidate
#     refuses these BOTH ways — we neither check them nor claim we did.
#   - `sed -i 's|a|b|' f` and friends: the segment splitter cuts on `|`, so a
#     pipe inside quotes shreds the command before the verb is read.
#   - a redirect whose operator is not at the start of its token.
#   - a write into a directory the SAME command creates: `mkdir -p out && …
#     > out/f` runs the mkdir after this hook, so `out` is not there yet and
#     the target reads as prose (sd-8a6w). Spell the write as a second call, or
#     into a directory that already exists, to have it gated.
#   - a `cd` we re-rooted wrongly: a stray `)` from a case arm or a nested
#     subshell can return the base to the payload's cwd early, and the check
#     then lands on a path the command was not writing.
# Every one of those ends the same way: the write proceeds unchecked. None of
# them ends with loto reporting a clean it did not establish, which is the line
# that matters (docs/DESIGN.md invariant 9).
#
# Among sibling subagents, each is a distinct loto owner at WRITE time: every
# check and beacon a subagent makes is stamped with the payload's agent_id
# (LOTO_SUBAGENT_ID) and checked with --gate, so sibling A's beacon refuses
# sibling B's write to the same path (loto-xwod, loto-wofb). They are still
# ONE owner at LOCK time — a worker's `loto lock` runs from Bash, which this
# hook cannot stamp — so lock-level exclusion between siblings remains
# dispatch-time write-set disjointness, and a parent-owned lock never refuses
# its own subagent (the binary treats it as kin, contract 3). A root session
# (no agent_id) runs plain `loto check`, unchanged.
#
# Permissive on infrastructure failure: if `loto` isn't installed or the check
# can't complete for non-conflict reasons, the hook allows the edit. We refuse
# to block on plumbing problems — only on real conflicts.

set -u

# LOTO_GATE_CONTRACT is the version of the hook<->binary interface THIS hook
# needs. A loto whose own GateContractVersion is lower prints one ⚠ row per
# session and carries on (loto-tzmv.7). Bump this in lockstep with the constant
# in internal/cli/gate_contract.go whenever the hook starts depending on
# something an older loto could not do.
#
#   1 — `check --gate`, `guard`, `beacon`, `check --cwd-unknown`.
#   2 — direct-CLI relative positionals resolve against the caller's cwd.
#   3 — `check --gate` and `beacon` under LOTO_SUBAGENT_ID treat the parent
#       identity's locks/claims as the sibling's own (loto-wofb). Without it,
#       the stamped check below refuses a worker on the lock it just took.
export LOTO_GATE_CONTRACT=3

# Read the tool_input envelope from stdin (Claude Code hook protocol).
input="$(cat 2>/dev/null || true)"

# session_id scopes the once-per-session notices below. Read with a bare shell
# match rather than jq, because one of the things we may have to announce is
# that jq is missing.
session_id=""
case "$input" in
  *'"session_id"'*)
    session_id="${input#*\"session_id\":\"}"
    session_id="${session_id%%\"*}"
    ;;
esac

# notice_once KEY LINE... — say something on stderr the first time per session,
# then stay quiet.
#
# ‡ Every path below that lets a tool call through WITHOUT consulting loto is a
# fail-open, and a silent fail-open is protection that evaporates invisibly —
# strictly worse than no gate, because the fleet still believes it is covered.
# That is not hypothetical: on 2026-08-12 the loto on PATH was 8 days stale and
# silently lacked a verb the hook needed, and nothing said so for 8 days
# (loto-tzmv.1 obligation (a), loto-tzmv.7).
#
# Once per session, because the gate runs on EVERY tool call: a missing binary
# would otherwise print on all of them, and a row that repeats hundreds of times
# is a row the model learns to skip.
_notice_dir="${TMPDIR:-/tmp}/loto-gate-notice"
notice_once() {
  local key="$1"; shift
  local marker
  if [ -n "$session_id" ]; then
    marker="$_notice_dir/${session_id//[^A-Za-z0-9._-]/_}.$key"
    mkdir -p "$_notice_dir" 2>/dev/null
    # O_EXCL via noclobber: the first caller creates it, the rest are refused.
    if ! (set -o noclobber; : >"$marker") 2>/dev/null; then
      return 0
    fi
  fi
  printf '%s\n' "$@" >&2
}

# unparsed VERB — say once per session that a write walked past the gate.
#
# The scan reads four shapes (sed -i, > / >>, tee, cp/mv/rm operands) and a
# short list of verbs that certainly write but whose operands it does not try
# to parse. For that list, silence would be the same evaporating protection
# notice_once exists to prevent — so we name the verb and admit the miss. The
# list is deliberately short: a warning that fires on every ordinary command is
# a warning the model learns to skip, so `make`, `python`, and every other
# maybe-writer stay silent and stay documented in the header instead.
unparsed() {
  notice_once bash-unparsed \
    "⚠ loto: '$1' writes in a shape this gate does not parse — that write was NOT checked against peers' locks" \
    "⚠ gated shapes: sed -i · > and >> · tee · cp/mv/rm · install · dd of= · a heredoc's own writes when fed to python/ruby/node/perl/bash/sh. Everything else is unchecked."
}

# Bail if loto isn't on PATH — but say so. Don't fail the tool call.
if ! command -v loto >/dev/null 2>&1; then
  notice_once loto-missing \
    "⚠ loto=not-on-PATH gate=fail-open" \
    "⚠ every write this session is unchecked — peers' locks are not being honored" \
    '```bash' \
    "cd ~/Projects/loto && make install" \
    '```'
  exit 0
fi

# jq parses every payload below. Without it the hook reads no paths at all and
# waves everything through, which looks exactly like "nothing was locked".
if ! command -v jq >/dev/null 2>&1; then
  notice_once jq-missing \
    "⚠ jq=not-on-PATH gate=fail-open" \
    "⚠ the hook cannot read tool payloads, so no path is being checked" \
    '```bash' \
    "brew install jq" \
    '```'
  exit 0
fi

if [ -z "$input" ]; then
  notice_once payload-empty "⚠ payload=empty gate=fail-open"
  exit 0
fi

# check_path PATH — runs `loto check` for one path. Echoes loto's output and
# returns loto's exit code (0 no-conflict, 1 conflict, else plumbing).
#
# Workaround for loto-d3l: `loto check /abs/path` canonicalizes differently than
# `loto check relative/path`, returning a false "no conflicts" for held files.
# Convert absolute paths to git-root-relative and run check from the git root.
# If we can't find a git root, fall back to the path as-is — better to allow
# than to block on plumbing.
#
# Use `git rev-parse --show-prefix` for the repo-relative dir, NOT a
# `${path#$groot/}` strip: that strip is case-SENSITIVE, so on a case-insensitive
# FS a lowercase worktree path (…/projects/…) won't match git's canonically-cased
# root (…/Projects/…). rel then stays absolute, `loto check /abs` returns a false
# "no conflicts" (loto-d3l), and this gate fails OPEN. show-prefix canonicalizes
# case and yields rel directly.
#
# ccp-0b4.1: `loto check` opens a per-project SQLite store — observed 5-10s wall
# clock under fleet contention, paid on EVERY Edit/Write/MultiEdit/NotebookEdit.
# Cache a CLEAN (rc=0) verdict per path for a short TTL so a burst of edits to
# the same file (e.g. one MultiEdit call, or several Edits in a row) pays the
# loto round-trip once instead of once per edit. Conflicts (rc=1) and plumbing
# failures are NEVER cached — a real block must always re-check, since staleness
# there would silently reopen the exact race this gate exists to close. The TTL
# is short (5s) so the blast radius of a missed peer-lock is bounded to "peer
# locks this exact path within 5s of our last clean check of it" — narrow
# compared to the alternative of gating on loto's internal store mtime, which
# doesn't reliably change on lock/unlock (WAL/mmap writes don't always touch
# the file's mtime; verified empirically, ccp-0b4.1).
_check_cache_ttl=5
_check_cachedir="${TMPDIR:-/tmp}/loto-check-cache"

# loto_check ARGS… — the one entry point to `loto check` for this hook. A subagent
# call (agent_id set) is stamped with its per-sibling identity — the SAME stamp
# mint_beacon uses, or a sibling's own beacon reads back as a peer's
# (loto-wofb) — and runs --gate, the deny surface built for exactly this
# (loto-vr2): any foreign live lock, beacon, or claim refuses. A root session
# stays on plain `loto check`; gating it would be a policy change nobody
# ratified.
loto_check() {
  if [ -n "${agent_id:-}" ]; then
    LOTO_SUBAGENT_ID="$agent_id" loto check --gate "$@"
  else
    loto check "$@"
  fi
}

# loto_status ARGS… — same stamping rule as loto_check, for `loto status`.
# Unlike check/check --gate, status reports EVERY overlapping holder of a
# target, self included — the only read this hook has for "do I already hold
# this" (sd-cpbj).
loto_status() {
  if [ -n "${agent_id:-}" ]; then
    LOTO_SUBAGENT_ID="$agent_id" loto status "$@"
  else
    loto status "$@"
  fi
}

check_path() {
  local path="${1:-}" dir groot rel out rc key cachefile now last age
  # hot path: pure-bash key + read builtin avoid tr/sed subshells (runs per tool call)
  # agent_id is part of the key: verdicts differ per sibling now, and one
  # shared entry would serve whichever verdict landed first to both.
  key="${agent_id:-}|${path}"
  key="${key//[^A-Za-z0-9._-]/_}"
  cachefile="$_check_cachedir/$key"
  now="$(date +%s)"
  if [ -f "$cachefile" ] && read -r last < "$cachefile" 2>/dev/null; then
    case "$last" in '' | *[!0-9]*) last=0 ;; esac
    age=$((now - last))
    if [ "$age" -ge 0 ] && [ "$age" -lt "$_check_cache_ttl" ]; then
      tail -n +2 "$cachefile" 2>/dev/null
      return 0
    fi
  fi

  if [ "${path#/}" != "$path" ]; then
    # Absolute: the token carries its own base, so the caller's cwd is
    # irrelevant — the rel we derive here is repo-rooted by construction.
    dir="$(dirname "$path")"
    if [ -d "$dir" ] && groot="$(cd "$dir" && git rev-parse --show-toplevel 2>/dev/null)"; then
      rel="$(cd "$dir" && git rev-parse --show-prefix 2>/dev/null)$(basename "$path")"
      out="$(cd "$groot" && loto_check "$rel" 2>&1)"
      rc=$?
    else
      out="$(loto_check "$path" 2>&1)"
      rc=$?
    fi
  else
    out="$(loto_check "$path" 2>&1)"
    rc=$?
  fi

  if [ "$rc" = "0" ]; then
    mkdir -p "$_check_cachedir" 2>/dev/null
    { printf '%s\n%s' "$now" "$out" >"$cachefile"; } 2>/dev/null
  else
    rm -f "$cachefile" 2>/dev/null
  fi

  printf '%s' "$out"
  return "$rc"
}

# lock_owned_by_me PATH — true (rc 0) when a live lock already covers PATH's
# exact path. Called ONLY after loto_check has denied PATH with kind=claim
# rows and no kind=lock row, which means no FOREIGN lock covers it (a
# kind=lock row is how `check`/`check --gate` name one, and neither ever
# reports the caller's own lock). So an overlap `loto status` finds here,
# under that precondition, can only be this session's own lock, or a kin
# subagent's (sd-cpbj) — the one read this hook has for "do I already hold
# this", since check/check --gate are both silent about a caller's own
# holdings by design.
#
# Never cached, unlike check_path's clean-verdict cache: this runs only on
# the rare claim-only deny path, and a released or newly-taken lock has to be
# seen on the very next call (AC: the refusal path re-reads current state).
#
# Resolution mirrors check_path's absolute-path handling (git-root-relative,
# run from the git root) rather than sharing code with it, matching this
# file's existing convention of mint_beacon resolving independently too.
lock_owned_by_me() {
  local path="${1:-}" dir groot rel out
  if [ "${path#/}" != "$path" ]; then
    dir="$(dirname "$path")"
    if [ -d "$dir" ] && groot="$(cd "$dir" && git rev-parse --show-toplevel 2>/dev/null)"; then
      rel="$(cd "$dir" && git rev-parse --show-prefix 2>/dev/null)$(basename "$path")"
      out="$(cd "$groot" && loto_status "$rel" 2>&1)"
    else
      out="$(loto_status "$path" 2>&1)"
    fi
  else
    out="$(loto_status "$path" 2>&1)"
  fi
  case "$out" in
    *'✗ overlap'*) return 0 ;;
  esac
  return 1
}

# mint_beacon PATH — announce that THIS subagent is about to write PATH, so a
# sibling subagent's next write to the same path is refused (loto-xwod).
#
# ‡ Subagents only, keyed on the payload's agent_id. Siblings of one session
# share LOTO_AGENT_ID and LOTO_SESSION_ID, so absent a per-sibling stamp they
# collapse to one owner and a lock reads as a re-entrant refresh — loto could
# not see them at all. On 2026-08-14 two subagents dispatched onto one bead
# wrote the same two files concurrently, both holding locks, neither blocked,
# and one's uncommitted work was then destroyed by a branch cut.
#
# ✗ NOT minted for a session with no agent_id. A session already has `loto
# lock` to declare territory with; minting on its behalf would make every
# ordinary Edit block every peer session for the beacon's TTL, which is a
# policy change nobody ratified. The ratified scope is per-subagent
# (loto-xwod, dk 2026-08-15).
#
# Fail-open, always: a beacon is an announcement, and a hook that cannot make
# one must not refuse the write. Errors and output are discarded, including
# `unknown command` from a loto older than the beacon verb.
mint_beacon() {
  local path="${1:-}" dir groot rel
  [ -n "$agent_id" ] || return 0
  [ -n "$path" ] || return 0
  # Last line of defence, not the first: every call site is supposed to have
  # run path_candidate already. One of them (the verb-arg loop) had not, and a
  # beacon on the literal token `$TMP/x` reached the store and needed reaping
  # (sd-xtx). A shell metacharacter here means the caller handed us a string
  # nobody expanded, so it names no file and must not become a lock. Narrower
  # than path_candidate on purpose — a space is legal in a real filename and
  # the Edit path (:656) passes real paths straight through.
  case "$path" in
    '~'*|*'$'*|*'*'*|*'?'*|*'['*|*'`'*) return 0 ;;
  esac
  if [ "${path#/}" != "$path" ]; then
    dir="$(dirname "$path")"
    if [ -d "$dir" ] && groot="$(cd "$dir" && git rev-parse --show-toplevel 2>/dev/null)"; then
      rel="$(cd "$dir" && git rev-parse --show-prefix 2>/dev/null)$(basename "$path")"
      (cd "$groot" && LOTO_SUBAGENT_ID="$agent_id" loto beacon "$rel") >/dev/null 2>&1
      return 0
    fi
  fi
  LOTO_SUBAGENT_ID="$agent_id" loto beacon "$path" >/dev/null 2>&1
  return 0
}

# path_candidate TOKEN — set $_pc to the literal path TOKEN names and return 0,
# or return 1 for a token whose meaning we cannot read off the command line.
#
# Strips one layer of matched quotes, then refuses anything that is not a plain
# literal: `$VAR`, globs, `~`, redirection leftovers, embedded quotes, device
# files. The filter is deliberately strict, and in BOTH directions:
#   - extracting `"a` out of a quoted sentence would name a path the caller
#     never wrote;
#   - a token we cannot expand (`$OUT`) has no literal meaning here, and
#     checking the string `$OUT` would return a clean verdict for a path we
#     never resolved — invariant 9's false clean, spelled a new way.
# So an unreadable token is SKIPPED, and skipping is a miss we own (see the
# coverage note in the header).
#
# Sets a global rather than echoing: this runs on every token of every Bash
# call, and a subshell per token is latency the whole fleet pays.
_pc=""
path_candidate() {
  local t="${1:-}"
  case "$t" in
    \'*\') t="${t#\'}"; t="${t%\'}" ;;
    \"*\") t="${t#\"}"; t="${t%\"}" ;;
  esac
  case "$t" in
    '' | -*) return 1 ;;
    /dev/*) return 1 ;;                # /dev/null is not a file a peer locks
    *[!A-Za-z0-9._/@+-]*) return 1 ;;  # expansions, globs, ~, quotes, spaces
  esac
  _pc="$t"
  return 0
}

# _first_quoted STR — on success, sets $_fqt to the text between the first
# matching pair of quotes in STR (whichever quote char opens first) and $_fqa
# to what follows the closing quote; returns 1 if STR has no complete quoted
# substring. Used by extract_quoted to pull a literal path argument out of a
# heredoc script line without a real language parser (sd-wgcw).
_first_quoted() {
  local s="$1" sp dp rest
  sp="${s%%\'*}"; dp="${s%%\"*}"
  if [ "$sp" = "$s" ] && [ "$dp" = "$s" ]; then
    return 1
  fi
  if [ "$dp" = "$s" ] || { [ "$sp" != "$s" ] && [ "${#sp}" -le "${#dp}" ]; }; then
    rest="${s#*\'}"
    case "$rest" in *\'*) : ;; *) return 1 ;; esac
    _fqt="${rest%%\'*}"
    _fqa="${rest#*\'}"
  else
    rest="${s#*\"}"
    case "$rest" in *\"*) : ;; *) return 1 ;; esac
    _fqt="${rest%%\"*}"
    _fqa="${rest#*\"}"
  fi
  return 0
}

# extract_quoted LINE — sets $_eq to the first quoted string in LINE that
# isn't a bare open()-mode token (r/w/a/x/r+/w+/a+/>/>>/</+</+>), so
# `open(path, 'w')` and Perl's `open($fh, '>', 'path')` both yield the real
# path rather than the mode. Tries at most 3 quoted tokens; a call with more
# mode-shaped leading args than that is not one we try to read.
extract_quoted() {
  local s="$1" tries=0
  _eq=""
  while [ "$tries" -lt 3 ]; do
    tries=$((tries + 1))
    _first_quoted "$s" || return 1
    case "$_fqt" in
      r|w|a|x|r+|w+|a+|'>'|'>>'|'<'|'+<'|'+>') s="$_fqa"; continue ;;
    esac
    _eq="$_fqt"
    return 0
  done
  return 1
}

# scan_script_write LINE SIGNAL... — if LINE contains any SIGNAL substring,
# extract its first plausible quoted path argument and gate it as a write
# candidate. Deliberately loose: a signal name that is actually a plain
# identifier, or a mode-carrying `open()` call the caller decided to scan
# that turns out to be a read, both pass through — false positives are
# accepted per this gate's conservative-scan contract (standard-checks.md); a real
# write must never be missed because the pattern was drawn too tight. A
# block still only happens if the extracted path is genuinely peer-held.
scan_script_write() {
  local line="$1"; shift
  local sig
  for sig in "$@"; do
    case "$line" in
      *"$sig"*)
        extract_quoted "$line" || return 0
        _eq="${_eq#>>}"; _eq="${_eq#>}"; _eq="${_eq#<}"   # Perl's 2-arg open($fh,'>path')
        path_candidate "$_eq" && gate_one "$_pc"
        return 0
        ;;
    esac
  done
}

# scan_heredoc_line LANG LINE — one line of a heredoc body fed to an
# interpreter this hook recognizes (sd-wgcw). bash/sh/dash/zsh never reach
# here — the caller lets those fall through to the ordinary shell scan
# instead, since a bash/sh heredoc body is itself shell. Everything else gets
# a short, per-language list of calls whose first literal argument is
# ordinarily the path being written. A write whose mode/verb this list does
# not name, or whose path is not a literal on this line, is a miss (see the
# header MISSES note) — not a false clean, since nothing here reports a
# verdict for a path it never resolved.
scan_heredoc_line() {
  local lang="$1" line="$2"
  case "$lang" in
    python|python3)
      case "$line" in
        *'open('*)
          case "$line" in
            *"'w'"*|*'"w"'*|*"'a'"*|*'"a"'*|*"'x'"*|*'"x"'*)
              scan_script_write "$line" "open("
              ;;
          esac
          ;;
      esac
      scan_script_write "$line" \
        "os.remove(" "os.unlink(" "os.rename(" \
        "shutil.copy(" "shutil.move(" "shutil.rmtree(" ".write_text("
      ;;
    ruby)
      case "$line" in
        *'File.open('*)
          case "$line" in
            *"'w'"*|*'"w"'*|*"'a'"*|*'"a"'*)
              scan_script_write "$line" "File.open("
              ;;
          esac
          ;;
      esac
      scan_script_write "$line" \
        "File.write(" "File.delete(" "File.unlink(" "IO.write(" \
        "FileUtils.rm(" "FileUtils.rm_f(" "FileUtils.cp(" "FileUtils.mv("
      ;;
    node|nodejs)
      scan_script_write "$line" \
        "writeFileSync(" "writeFile(" "appendFileSync(" "appendFile(" \
        "unlinkSync(" "unlink(" "renameSync(" "rename(" "copyFileSync(" "copyFile("
      ;;
    perl)
      case "$line" in
        *'open('*)
          case "$line" in
            *"'>"*|*'">'*)
              scan_script_write "$line" "open("
              ;;
          esac
          ;;
      esac
      scan_script_write "$line" "unlink"
      ;;
  esac
}

# gate_one TOKEN — gate ONE extracted write target: re-root it against any `cd`
# seen earlier in the same command, skip what cannot be resolved, block on a
# peer's lock, announce the write otherwise. Shell scan only; the Edit path
# still calls check_path directly.
gate_one() {
  local tok="${1:-}" out rc
  [ -n "$tok" ] || return 0
  # `(cd x && rm y)` — the subshell's closing paren rides along on the last
  # token, and `rm y)` names no file called `y)`.
  while [ "${tok%\)}" != "$tok" ]; do tok="${tok%\)}"; done
  if [ "${tok#/}" = "$tok" ]; then
    # A leading `cd` moved the base out from under the payload's cwd
    # (docs/DESIGN.md: a Bash command's base is the payload cwd PLUS any
    # leading cd in the same command). A literal cd is re-rooted here. A cd we
    # could not read is a base we cannot vouch for: the scan fails OPEN as it
    # always has and we say so once, rather than inventing a refusal that would
    # block every `cd "$d" && rm x`.
    if [ -n "$_seg_base" ]; then
      tok="$_seg_base/$tok"
    elif [ "$_base_unknown" = 1 ]; then
      notice_once bash-cd-unknown \
        "⚠ loto: a Bash command cd'd somewhere its payload does not name — its relative writes went unchecked" \
        "⚠ spell the path in full (\$(git rev-parse --show-toplevel)/…) to have it gated"
      return 0
    fi
  fi
  # Only check things that resolve to something real: the token itself, or —
  # for a file this command is about to create — the directory it would land
  # in. Everything else is a word, not a path.
  #
  # The escape hatch here used to be "contains a slash", and a slash is not
  # evidence of a path (sd-8a6w). The segment splitter cuts a command on
  # `;|&` and newlines, so a prose sentence carried inside one — a commit
  # message, a bead body, a heredoc line — arrives as segments, and a
  # sentence whose first word happens to be a write verb hands this scan its
  # nouns: `install the test to version/loto` beaconed `version/loto`, which
  # then had to be reaped out of the store by hand. `the`, `test` and `to`
  # were already dropped for not existing; the two-word fragment was not.
  #
  # Requiring the parent directory keeps the case the slash rule was there
  # for — `echo hi > internal/store/new.go` names a file that does not exist
  # yet and must still be gated — while a prose fragment, whose first
  # component names no directory, is dropped. A dropped token is a miss we
  # own (header MISSES note), never a false clean: the gate reports no
  # verdict for a path it never resolved.
  if [ ! -e "$tok" ] && [ ! -d "${tok%/*}" ]; then
    return 0
  fi
  out="$(check_path "$tok")"
  rc=$?
  if [ "$rc" = "1" ]; then
    set +f
    handle_deny "$tok" "$out"
  fi
  # Clean — this subagent is about to touch it, so say so before it does.
  [ "$rc" = "0" ] && mint_beacon "$tok"
  return 0
}

# block PATH OUTPUT — emit the blocker rows to the model and refuse the call.
block() {
  {
    echo "✗ loto: blocked — peer holds this file."
    echo "$2"
    echo ""
    echo "Options: wait, work elsewhere, or 'loto unlock $1 --force -t \"why\"' to take over."
  } >&2
  exit 2
}

# block_claim_no_lock PATH — refuse an edit under a peer's directory claim,
# for the session that holds no lock of its own on PATH. A claim has no
# takeover verb (render.EmitGateDeny: "options=wait|pick-other-work|
# message-holder") and does not block a lock beneath it (standard-tools.md,
# and `loto claim`'s own "advisory: claim does not block lock/check under the
# prefix" line) — so the remedy is to take the file's own lock, never to name
# or fight the claim (sd-cpbj AC: "names the lock to take, not the claim").
block_claim_no_lock() {
  {
    echo "✗ loto: blocked — a peer's directory claim covers this path, and this session holds no lock on it."
    echo ""
    echo "A claim never blocks a lock beneath it. Options: wait, work elsewhere, or take your own lock first:"
    echo '```bash'
    echo "loto lock $1 -t \"why\""
    echo '```'
  } >&2
  exit 2
}

# handle_deny PATH OUT — called once loto_check has denied PATH (rc=1), with
# its rendered deny rows in OUT. A kind=lock row (render.GateKindLock) is a
# foreign live exclusive lock or beacon — always blocks, unchanged from
# before this function existed. A deny made ONLY of kind=claim rows names a
# peer's directory claim, which is documented as non-blocking for a lock or
# check beneath it — so it must not out-rank a lock THIS session already
# holds on the exact file (sd-cpbj). check/check --gate never report the
# caller's own lock either way, so lock_owned_by_me's fresh `loto status`
# read is what tells "nobody holds it" (still refused) from "I do" (allowed,
# override the claim).
handle_deny() {
  local path="$1" out="$2"
  case "$out" in
    *'kind=lock'*) block "$path" "$out" ;;
  esac
  if lock_owned_by_me "$path"; then
    mint_beacon "$path"
    return 0
  fi
  block_claim_no_lock "$path"
}

tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null)"

# agent_id is present ONLY inside a subagent call, and is distinct per sibling
# (code.claude.com/docs/en/hooks). session_id is shared across the whole
# session, which is precisely why it cannot key a beacon. Empty here means "not
# a subagent" and mint_beacon no-ops.
agent_id="$(printf '%s' "$input" | jq -r '.agent_id // empty' 2>/dev/null)"

# --- Shell tools: scan the command's writes for peer-held paths. -------------
# Bash gets a fresh process per call, so the payload's cwd plus any leading
# `cd` in the same command is a sound base for a relative token (docs/DESIGN.md
# invariant 9). A shell tool with its own persistent cwd is not — see the
# header note.
if [ "$tool" = "Bash" ]; then
  cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null)"
  if [ -z "$cmd" ] || [ "$cmd" = "null" ]; then
    exit 0
  fi

  set -f  # no glob expansion during the best-effort word-split below
  # Split compound commands into simple segments on ; | & and newlines, then
  # scan each segment twice: once for a redirect target (which writes whatever
  # the verb is), once for the verb's own write operands. tr maps each
  # separator char to a newline.
  segs="$(printf '%s' "$cmd" | tr ';|&\n' '\n\n\n\n')"
  _seg_base=""      # literal dir a `cd` moved us to; "" = the payload's cwd
  _base_unknown=0   # a `cd` whose target we could not read literally
  _heredoc=""       # delimiter we are skipping to; "" = not inside a body
  _heredoc_lang=""  # interpreter the heredoc feeds, or "" = treat body as data
  _close=0          # the segment just scanned closed a subshell
  while IFS= read -r seg; do
    seg="${seg#"${seg%%[![:space:]]*}"}"   # ltrim
    [ -n "$seg" ] || continue

    # A subshell's `cd` dies with its `)`, so the base returns to the payload's
    # cwd once the segment carrying the `)` has been scanned. Deferred, because
    # the `rm` in `(cd x && rm y)` is still inside the subshell.
    if [ "$_close" = 1 ]; then
      _seg_base=""; _base_unknown=0; _close=0
    fi

    # A heredoc body is DATA, not commands — UNLESS the heredoc feeds an
    # interpreter that will execute it (python/ruby/node/perl/bash/sh, read
    # off the verb the `<<` follows below; sd-wgcw). `cat > f <<EOF` has had
    # its redirect gated by the time the body arrives, and scanning THAT body
    # would check every path the file happens to MENTION — a script being
    # written that contains the line `rm internal/store/locks.go` is not a
    # write to that file. So a heredoc with no recognized interpreter stays
    # data, exactly as before; skipping it removes false positives and adds
    # no misses.
    if [ -n "$_heredoc" ]; then
      if [ "${seg%%[[:space:]]*}" = "$_heredoc" ]; then
        _heredoc=""; _heredoc_lang=""
        continue
      fi
      case "$_heredoc_lang" in
        bash|sh|dash|zsh) : ;;   # itself shell — fall through to the ordinary scan below
        "") continue ;;         # no recognized interpreter — body is data
        *) scan_heredoc_line "$_heredoc_lang" "$seg"; continue ;;
      esac
    fi
    # `$(…)` closes a paren without ending a subshell, so it must not reset the
    # base — `cd x && rm $(ls)` is still standing in x.
    case "$seg" in
      *'$('*) : ;;
      *')'*) _close=1 ;;
    esac

    case "$seg" in
      *'<<'*)
        _heredoc="${seg#*<<}"
        _heredoc="${_heredoc#-}"                       # <<- tab-stripping form
        _heredoc="${_heredoc%%[[:space:]]*}"
        _heredoc="${_heredoc#\'}"; _heredoc="${_heredoc%\'}"
        _heredoc="${_heredoc#\"}"; _heredoc="${_heredoc%\"}"
        case "$_heredoc" in '<'*) _heredoc="" ;; esac   # <<< is a herestring
        # Which interpreter (if any) this heredoc feeds, read off the verb
        # the `<<` follows — `python3 <<'EOF'` names python3, `cat <<EOF`
        # names none, so the body stays data (sd-wgcw).
        _heredoc_lang=""
        if [ -n "$_heredoc" ]; then
          _hd_verb="${seg%%[[:space:]]*}"
          _hd_verb="${_hd_verb#\(}"
          _hd_verb="${_hd_verb##*/}"
          case "$_hd_verb" in
            python|python3|ruby|node|nodejs|perl|bash|sh|dash|zsh) _heredoc_lang="$_hd_verb" ;;
          esac
        fi
        ;;
    esac

    # --- (a) output redirect: `> f`, `>>f`, `2> f`. -------------------------
    # A redirect writes its target regardless of the verb in front of it, so
    # this pass reads the whole segment, verb included: `&> f` arrives here as
    # a segment starting with `>` (tr already split the `&` off).
    _pending=0
    for tok in $seg; do
      # The operator must START the token (`>f`, `>>f`, `2>f`) or be the whole
      # of it. Anything else that merely CONTAINS a `>` — `a->b`, `x=>y`, an
      # arithmetic `$((1>2))` — is prose, and a loose match here would name
      # paths the caller never wrote.
      case "$tok" in
        '>'*|[0-9]'>'*) _rt="${tok##*>}" ;;
        *)
          [ "$_pending" = 1 ] || continue
          _pending=0
          path_candidate "$tok" && gate_one "$_pc"
          continue
          ;;
      esac
      if [ -n "$_rt" ]; then
        _pending=0
        path_candidate "$_rt" && gate_one "$_pc"
      else
        _pending=1                       # `> f` — the target is the next token
      fi
    done

    # --- (b) the verb's own write operands. ---------------------------------
    verb="${seg%%[[:space:]]*}"            # first word
    verb="${verb#\(}"                     # `(cd x && …)` subshell
    verb="${verb##*/}"                     # strip any path prefix (/bin/mv → mv)
    args="${seg#*[[:space:]]}"             # everything after the verb
    [ "$args" = "$seg" ] && args=""        # verb had no operands
    case "$verb" in
      mv|cp|rm) : ;;
      tee)
        # tee writes every operand it is given; -a only changes how.
        _list=""
        for tok in $args; do
          case "$tok" in -*) continue ;; esac
          path_candidate "$tok" && _list="$_list $_pc"
        done
        args="$_list"
        ;;
      sed)
        # `sed -i` writes its operands in place. Plain sed is a read — its
        # output only lands somewhere through a redirect, and pass (a) has
        # that. The first positional is the SCRIPT unless -e/-f supplied one,
        # and BSD sed spells the backup suffix as a separate empty token
        # (`sed -i '' 's/x/y/' f`), so both are stepped over here: mistaking a
        # script for a filename is how this pass would start blocking sessions
        # over an `s/a/b/` that names no file at all.
        _inplace=0 _have_e=0
        for tok in $args; do
          case "$tok" in
            --in-place|--in-place=*) _inplace=1 ;;
            --*) : ;;
            -*i*) _inplace=1 ;;
          esac
          case "$tok" in
            -e*|--expression*|-f*|--file*) _have_e=1 ;;
          esac
        done
        [ "$_inplace" = 1 ] || continue
        _list="" _skip=0 _after_i=0 _script=0
        for tok in $args; do
          if [ "$_skip" = 1 ]; then _skip=0; continue; fi
          case "$tok" in
            -e|--expression|-f|--file) _skip=1; continue ;;
            -i) _after_i=1; continue ;;
            -*) continue ;;
          esac
          if [ "$_after_i" = 1 ]; then
            _after_i=0
            case "$tok" in "''"|'""') continue ;; esac   # BSD backup suffix
          fi
          if [ "$_have_e" = 0 ] && [ "$_script" = 0 ]; then
            _script=1; continue                          # the lone script arg
          fi
          path_candidate "$tok" && _list="$_list $_pc"
        done
        args="$_list"
        ;;
      cd|pushd)
        # Re-root what follows, or mark the base unreadable. gate_one decides
        # what an unreadable base costs; recording it is this pass's job.
        _tok="${args%%[[:space:]]*}"
        if [ -z "$args" ] || ! path_candidate "$_tok"; then
          _seg_base=""; _base_unknown=1              # `cd`, `cd -`, `cd "$d"`
        elif [ "${_pc#/}" != "$_pc" ]; then
          _seg_base="$_pc"; _base_unknown=0          # absolute: base is known
        elif [ "$_base_unknown" = 1 ]; then
          :                                          # relative onto unknown
        elif [ -n "$_seg_base" ]; then
          _seg_base="$_seg_base/$_pc"
        else
          _seg_base="$_pc"
        fi
        continue
        ;;
      popd) _seg_base=""; _base_unknown=1; continue ;;
      git)
        sub="${args%%[[:space:]]*}"
        case "$sub" in
          mv|rm) args="${args#*[[:space:]]}" ;;
          apply|checkout|restore|stash|clean) unparsed "git $sub"; continue ;;
          *) continue ;;
        esac
        ;;
      install)
        # Conservative, like tee: treat every non-flag operand as a possible
        # write target. `install SRC DEST` has SRC in that list too — a
        # tolerated false positive (standard-checks.md), not a miss, and it only ever
        # turns into a block if SRC itself happens to be peer-held.
        _list=""
        for tok in $args; do
          case "$tok" in -*) continue ;; esac
          path_candidate "$tok" && _list="$_list $_pc"
        done
        args="$_list"
        ;;
      dd)
        # dd's only literal write operand is `of=FILE`; `if=FILE` is a read
        # and must not be checked as one, or `dd if=<peer-held> of=/tmp/x`
        # would block a call that never touches the peer's copy.
        _list=""
        for tok in $args; do
          case "$tok" in
            of=*) path_candidate "${tok#of=}" && _list="$_list $_pc" ;;
          esac
        done
        if [ -z "$_list" ]; then
          unparsed "$verb"
          continue
        fi
        args="$_list"
        ;;
      patch|truncate|ed|ex|shred|make) unparsed "$verb"; continue ;;
      awk|perl)
        # Only the in-place forms; a plain `awk '{print}'` writes nothing.
        case " $args " in
          *' -i'*|*' -pi'*|*'inplace'*) unparsed "$verb" ;;
        esac
        continue
        ;;
      *) continue ;;
    esac
    # path_candidate first, exactly as the cd (:522) and redirect (:528) sites
    # do. Without it this loop handed gate_one raw tokens, and any token with a
    # slash skipped the `-e` test at :390 and reached check_path — so `mv $TMP/x
    # dst` checked, and then BEACONED, the literal string `$TMP/x` (sd-xtx).
    # A verdict on a path we never resolved is invariant 9's false clean.
    for tok in $args; do
      case "$tok" in -*|"") continue ;; esac
      # gate_one strips a subshell's closing paren off the last token; do it
      # here too, because path_candidate now runs first and `a.go)` is not a
      # plain literal to it.
      while [ "${tok%\)}" != "$tok" ]; do tok="${tok%\)}"; done
      path_candidate "$tok" && gate_one "$_pc"
    done
  done <<EOF
$segs
EOF
  set +f
  exit 0
fi

# --- Edit/Write/MultiEdit/NotebookEdit: gate the single target path. ---------
# Edit/Write/MultiEdit use .tool_input.file_path; NotebookEdit uses notebook_path.
path="$(printf '%s' "$input" | jq -r '
  .tool_input.file_path
  // .tool_input.notebook_path
  // empty
' 2>/dev/null)"

# No path → nothing to check (unfamiliar tool shape). Allow.
if [ -z "$path" ] || [ "$path" = "null" ]; then
  exit 0
fi

out="$(check_path "$path")"
rc=$?

# loto exit codes per NORTH_STAR.md:
#   0 = no conflict, 1 = advisory conflict, 2 = usage, 3 = IO/system.
case "$rc" in
  0) mint_beacon "$path"; exit 0 ;;
  1) handle_deny "$path" "$out"; exit 0 ;;
  *)
    # Usage/IO/unknown — don't block on plumbing, but don't hide it either.
    # loto emits its own ⚠ for a store it cannot reach (loto-tzmv.8); this
    # covers the rest, including the usage error a contract mismatch produces.
    notice_once "loto-rc-$rc" "⚠ loto=exit-$rc gate=fail-open path=$path"
    exit 0
    ;;
esac
