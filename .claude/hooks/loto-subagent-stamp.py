#!/usr/bin/env python3
"""loto-wbkn: stamp a /team subagent's distinct CC agent_id into LOTO_SUBAGENT_ID.

A /team subagent inherits its parent's LOTO_AGENT_ID, so every sibling collapses
onto one loto owner_uuid (the loto-fs84 bug). The PreToolUse hook stdin carries
`agent_id` — distinct per sibling, null at the root session. This hook forwards
it as LOTO_SUBAGENT_ID to a cooperative `loto lock --shared`, so loto records a
distinct owner per sibling on the file.

BEACON, not GATE: this records an honest *shared* lock and NEVER denies an edit
(always exits 0). A SHARED lock is deliberate — an exclusive lock strips the
target's write bit (stripAll), which would chmod the file read-only immediately
before Claude's edit tool writes it → EACCES on the very edit this hook only
means to stamp (loto-25be). Shared locks leave the write bit untouched and two
different-owner shared rows coexist, so the hook can't serialize and doesn't try
to: it leaves a per-sibling trail that a reader surfaces — a file carrying ≥2
live owners = two siblings touched it. Detection lives in the reader
(`loto status` / a dispatch preflight), not in this lock.

It is a backstop to dispatch write-set partitioning (the actual loto-fs84 fix),
never load-bearing — the agent_id field is undocumented and may vanish on a CC
upgrade — so it fails open on every error and on an absent agent_id.

Enable (opt-in; this is NOT wired into loto's own settings.json because the
per-edit hook spawn taxes every native edit even at the root, where it no-ops):

    "PreToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          { "type": "command",
            "command": "python3 \"$CLAUDE_PROJECT_DIR/.claude/hooks/loto-subagent-stamp.py\"" }
        ]
      }
    ]

Note: this leaves a shared lock per file a subagent edits and does not
auto-unlock; loto's lock TTL self-heal reclaims stale rows, and /team's
unlock-by-path discipline still applies. Shared rows never strip the write bit,
so no chmod restore is owed on cleanup.
"""
import json
import os
import subprocess
import sys


def main() -> int:
    # The whole body is guarded: STAMP never denies an edit, so ANY failure —
    # unparseable stdin, a non-dict payload/tool_input (.get would raise
    # AttributeError), or a subprocess error — must fail open with exit 0.
    try:
        payload = json.load(sys.stdin)
        if not isinstance(payload, dict):
            return 0

        agent_id = payload.get("agent_id") or ""
        tool_input = payload.get("tool_input") or {}
        if not isinstance(tool_input, dict):
            return 0
        file_path = tool_input.get("file_path") or ""

        # Root session (agent_id null) or a non-file edit → nothing to stamp.
        if not agent_id or not file_path:
            return 0

        env = dict(os.environ, LOTO_SUBAGENT_ID=agent_id)
        subprocess.run(
            ["loto", "lock", "--shared", file_path, "-t", "subagent edit stamp"],
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=10,
        )
    except Exception:
        pass  # any failure is swallowed — the stamp must never block an edit.
    return 0


if __name__ == "__main__":
    sys.exit(main())
