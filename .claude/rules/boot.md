# Boot
updated: 2026-05-31

→ merge #162 (loto-u7b7) once linux rerun green, then `/team impl 3` for loto-l3as, loto-8yst, loto-pduc.

✓ done
- shipped #163/#164/#165 (loto-0gsu/qv91/3c7y) → merged to main, beads closed, worktrees pruned.

‡ traps
- #162 linux flaked on identity `TestEnsureForSessionRespectsCtxCancel` (tight <50ms vs 94ms under -race) — unrelated to whoami change; reran. Real-ish: ctx.Done() not prioritized over poll timer → loto-qqy5.
- LOTO_AGENT_ID unset until #162 merges + `go install ./cmd/loto`. Expected.
