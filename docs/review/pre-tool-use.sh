# Snapshot. Source of truth: ~/Projects/sdlc/plugins/loto/scripts/pre-tool-use.sh. Copied 2026-08-20 for the review bundle.
#!/usr/bin/env bash
# loto PreToolUse gate. Runs `loto check <path>` against the file(s) a tool is
# about to touch. Exit 2 (with reason on stderr) blocks the tool call;
# exit 0 lets it proceed.
#
# Two tool shapes are gated:
#   - Edit/Write/MultiEdit/NotebookEdit — the single path in .tool_input.
#   - Bash AND mcp__trixi__agent_shell — destructive relocations/deletions
#     (mv/cp/rm, git mv/rm) parsed out of .tool_input.command. This closes the
#     asymmetry where an `Edit` of a peer-held file is refused but a
#     `bash mv peer/file dest` was not (the mechanism behind
#     INCIDENT-ferret-t5d, cross-session variant).
#
# agent_shell runs arbitrary shell — including git checkout/restore/stash — and
# names its command field `.tool_input.command`, exactly as Bash does, so ONE
# scan serves both call sites (loto-tzmv.5; the two-call-sites-one-predicate
# shape the xgce spike recommended, nug 51f9ac1c006c). Until it was matched
# here, every write through agent_shell skipped the gate entirely — a second
# door standing open while the tzmv map builds the first. MCP tool names are
# valid PreToolUse matchers; settings.json already matches mcp__trixi__get_nug.
#
# ‡ agent_shell keeps its OWN persistent cwd, which the hook payload does not
# carry, so a relative token there has no single meaning. This hook therefore
# passes `loto check --cwd-unknown` for agent_shell and NOT for Bash, and loto
# refuses the token instead of resolving it against a base neither side can
# vouch for (loto-yexz; binary side shipped in #247). Absolute paths are
# unaffected — they carry their own base. This is the one place in the scan
# that does not fail open, and deliberately so: the old behavior returned a
# POSITIVE clean verdict that was false, which is worse than no check at all.
#
# The Bash scan is a deliberately CONSERVATIVE tripwire that fails OPEN: it acts
# only when it can see a destructive verb with a literal path operand that loto
# reports peer-held. Shell is not parsed robustly here (no globs, subshells, or
# quoted spaces) — that belongs in the loto binary (a `loto guard-bash` verb,
# tracked separately). Within a /team wave it is a NO-OP by design: wave agents
# share one loto identity, and a lock never blocks its own owner (loto-fs84), so
# this guards OTHER sessions only — same scope as the Edit gate.
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
export LOTO_GATE_CONTRACT=1

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

# Extra flags handed to `loto check` for THIS tool call. Set once at dispatch,
# below, from the payload's tool_name — see the --cwd-unknown block there.
# A plain string, not an array: macOS ships bash 3.2, where expanding an empty
# array under `set -u` is an unbound-variable error. The value is a single
# space-free token, so the unquoted expansion below is safe.
_check_flags=""

check_path() {
  local path="${1:-}" dir groot rel out rc key cachefile now last age
  # hot path: pure-bash key + read builtin avoid tr/sed subshells (runs per tool call)
  # The flags are part of the key: the same token can be clean under Bash's
  # resolution rule and refused under agent_shell's, so a verdict cached by one
  # tool must never be served to the other (loto-yexz).
  key="${_check_flags}${path}"
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
    # irrelevant and --cwd-unknown must NOT be passed — the rel we derive here
    # is repo-rooted by construction, and refusing it would block every
    # agent_shell call that spells its paths out in full.
    dir="$(dirname "$path")"
    if [ -d "$dir" ] && groot="$(cd "$dir" && git rev-parse --show-toplevel 2>/dev/null)"; then
      rel="$(cd "$dir" && git rev-parse --show-prefix 2>/dev/null)$(basename "$path")"
      out="$(cd "$groot" && loto check "$rel" 2>&1)"
      rc=$?
    else
      out="$(loto check "$path" 2>&1)"
      rc=$?
    fi
  else
    # shellcheck disable=SC2086 # intentional split: empty string must add no arg
    out="$(loto check $_check_flags "$path" 2>&1)"
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
  if [ "${path#/}" != "$path" ]; then
    dir="$(dirname "$path")"
    if [ -d "$dir" ] && groot="$(cd "$dir" && git rev-parse --show-toplevel 2>/dev/null)"; then
      rel="$(cd "$dir" && git rev-parse --show-prefix 2>/dev/null)$(basename "$path")"
      (cd "$groot" && LOTO_SUBAGENT_ID="$agent_id" loto beacon "$rel") >/dev/null 2>&1
      return 0
    fi
  fi
  # Relative token under agent_shell never reaches here — check_path refuses it
  # first (loto-yexz) — so the payload cwd is a sound base for what remains.
  LOTO_SUBAGENT_ID="$agent_id" loto beacon "$path" >/dev/null 2>&1
  return 0
}

# block PATH OUTPUT — emit the blocker rows to the model and refuse the call.
block() {
  # A --cwd-unknown refusal is NOT a peer conflict — loto declined to answer
  # because the token has no single meaning from here. Naming a holder that
  # was never consulted would send the agent to force-unlock a file nobody
  # holds, so the two exits carry different prose (loto-yexz).
  case "$2" in
    *relative-path-caller-cwd-unknown*)
      {
        echo "✗ loto: blocked — relative path, and this shell's cwd is not in the hook payload."
        echo "$2"
        echo ""
        echo "Options: re-run with an absolute path (\$(git rev-parse --show-toplevel)/$1)."
      } >&2
      exit 2
      ;;
  esac
  {
    echo "✗ loto: blocked — peer holds this file."
    echo "$2"
    echo ""
    echo "Options: wait, work elsewhere, or 'loto unlock $1 --force -t \"why\"' to take over."
  } >&2
  exit 2
}

tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null)"

# agent_id is present ONLY inside a subagent call, and is distinct per sibling
# (code.claude.com/docs/en/hooks). session_id is shared across the whole
# session, which is precisely why it cannot key a beacon. Empty here means "not
# a subagent" and mint_beacon no-ops.
agent_id="$(printf '%s' "$input" | jq -r '.agent_id // empty' 2>/dev/null)"

# --- Shell tools: scan destructive ops for peer-held paths. ------------------
# Bash and agent_shell share the `.tool_input.command` field name, so one scan
# covers both without a second parser (loto-tzmv.5).
if [ "$tool" = "Bash" ] || [ "$tool" = "mcp__trixi__agent_shell" ]; then
  # ‡ The resolution rule differs per tool, and the difference is the whole
  # point of the flag (docs/DESIGN.md invariant 9). Bash gets a fresh process
  # per call, so the payload's cwd plus any leading `cd` in the same command is
  # sound — no flag. agent_shell keeps its own persistent cwd, set by a `cd`
  # that may have happened three tool calls ago and absent from the payload, so
  # a relative token there gets refused rather than resolved against a base we
  # cannot vouch for. Without this line the binary side shipped in #247 is
  # inert and the false clean stays live (loto-yexz, loto-tzmv.10).
  if [ "$tool" = "mcp__trixi__agent_shell" ]; then
    _check_flags="--cwd-unknown"
  fi

  cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null)"
  if [ -z "$cmd" ] || [ "$cmd" = "null" ]; then
    exit 0
  fi

  set -f  # no glob expansion during the best-effort word-split below
  # Split compound commands into simple segments on ; | & and newlines, then
  # scan each segment's leading verb. tr maps each separator char to a newline.
  segs="$(printf '%s' "$cmd" | tr ';|&\n' '\n\n\n\n')"
  while IFS= read -r seg; do
    seg="${seg#"${seg%%[![:space:]]*}"}"   # ltrim
    [ -n "$seg" ] || continue
    verb="${seg%%[[:space:]]*}"            # first word
    verb="${verb##*/}"                     # strip any path prefix (/bin/mv → mv)
    args="${seg#*[[:space:]]}"             # everything after the verb
    [ "$args" = "$seg" ] && args=""        # verb had no operands
    case "$verb" in
      mv|cp|rm) : ;;
      git)
        sub="${args%%[[:space:]]*}"
        case "$sub" in
          mv|rm) args="${args#*[[:space:]]}" ;;
          *) continue ;;
        esac
        ;;
      *) continue ;;
    esac
    for tok in $args; do
      case "$tok" in -*|"") continue ;; esac
      # Only check things that exist locally or that at least look like a path
      # (contain a slash). Bare new-dest filenames are skipped — fail open.
      #
      # ‡ Under --cwd-unknown that existence test is worse than useless: `-e`
      # probes the PAYLOAD's cwd, not the shell's, so a bare `locks.go` typed
      # from an agent_shell sitting in internal/store tests false, has no
      # slash, and gets skipped before loto is ever consulted. That skip is how
      # the reproduced false clean survived #247 — the binary refuses the token
      # correctly, but nothing handed it over. Relative operands of a
      # destructive verb therefore go straight to check_path here (loto-yexz).
      if [ -n "$_check_flags" ] && [ "${tok#/}" = "$tok" ]; then
        : # relative + cwd unknown → always consult loto, it will refuse
      elif [ ! -e "$tok" ]; then
        case "$tok" in */*) : ;; *) continue ;; esac
      fi
      out="$(check_path "$tok")"
      rc=$?
      if [ "$rc" = "1" ]; then
        set +f
        block "$tok" "$out"
      fi
      # Clean — this subagent is about to touch it, so say so before it does.
      [ "$rc" = "0" ] && mint_beacon "$tok"
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
  1) block "$path" "$out" ;;
  *)
    # Usage/IO/unknown — don't block on plumbing, but don't hide it either.
    # loto emits its own ⚠ for a store it cannot reach (loto-tzmv.8); this
    # covers the rest, including the usage error a contract mismatch produces.
    notice_once "loto-rc-$rc" "⚠ loto=exit-$rc gate=fail-open path=$path"
    exit 0
    ;;
esac
