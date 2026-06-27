#!/usr/bin/env python3
"""loto-wbkn: stamp a /team subagent's distinct CC agent_id into LOTO_SUBAGENT_ID.

A /team subagent inherits its parent's LOTO_AGENT_ID, so every sibling collapses
onto one loto owner_uuid; loto then reads a sibling's lock as a re-entrant TTL
refresh and never serializes the collision (the loto-fs84 bug). The PreToolUse
hook stdin carries `agent_id` — distinct per sibling, null at the root session.
This hook forwards it as LOTO_SUBAGENT_ID to a cooperative `loto lock`, so loto
mints a distinct owner per sibling and its existing conflict logic serializes
them.

STAMP, not GATE: this records an honest lock and NEVER denies an edit (always
exits 0). It is a backstop to dispatch write-set partitioning, never
load-bearing — the agent_id field is undocumented and may vanish on a CC
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

Note: this auto-locks files as subagents edit them and does not auto-unlock;
loto's lock TTL self-heal reclaims stale rows, and /team's unlock-by-path
discipline still applies.
"""
import json
import os
import subprocess
import sys


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except Exception:
        return 0  # unparseable stdin → fail open

    agent_id = payload.get("agent_id") or ""
    tool_input = payload.get("tool_input") or {}
    file_path = tool_input.get("file_path") or ""

    # Root session (agent_id null) or a non-file edit → nothing to stamp.
    if not agent_id or not file_path:
        return 0

    env = dict(os.environ, LOTO_SUBAGENT_ID=agent_id)
    try:
        subprocess.run(
            ["loto", "lock", file_path, "-t", "subagent edit stamp"],
            env=env,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=10,
        )
    except Exception:
        pass  # STAMP never denies an edit; any failure is swallowed.
    return 0


if __name__ == "__main__":
    sys.exit(main())
