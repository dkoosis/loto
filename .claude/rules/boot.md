# Boot
updated: 2026-04-29

→ pick from `bd ready`. Top of queue: loto-lgk epic remainder (.2 zombie staleness, .6 inbox index — both P2, each needs design pass before code).

✓ done
- loto-lgk.7 shipped: MsgID/ThreadID + idempotent dedupe-on-append
- loto-lgk.5 shipped: AckRequired/ReadAt/Importance fields + read-side stamping
- closed .1, .3, .4 as already-shipped (verify before re-doing in this epic)

‡ traps
- before implementing any loto-lgk.* bead, grep mailbox.go/loto.go/cmd — three were already done
- `CLAUDE_SESSION_ID` ✗ in Bash env; only `CLAUDECODE`/`CLAUDE_CODE_*`
