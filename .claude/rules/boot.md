# Boot
updated: 2026-04-29

→ `bd ready` — 6 bugs in queue, top: loto-616 (P1 mailbox race).

‡ traps
- `CLAUDE_SESSION_ID` ✗ in Bash tool env — only `CLAUDECODE`/`CLAUDE_CODE_*`. Stable ID comes from CC session JSONL discovery (4622ab9).
- `try file` (no `--hold`) auto-releases on exit; use `--hold` in tests needing live lock
