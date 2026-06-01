# Boot
updated: 2026-05-31

→ `/team impl 4` to clear ready queue: loto-l3as (P2 SessionEnd hook), loto-8yst, loto-pduc, loto-qqy5 (all P3 bug). 0 open PRs.

✓ done
- shipped #162–#165 (loto-u7b7/0gsu/qv91/3c7y) → all merged to main, beads closed, worktrees+branches pruned. `go install ./cmd/loto` done → LOTO_AGENT_ID now populates.

‡ traps
- loto-qqy5: identity `awaitOrRecoverSession` doesn't prioritize ctx.Done() over the 5ms poll timer → cancellation lags under -race load, flakes `TestEnsureForSessionRespectsCtxCancel` (<50ms). Fix = non-blocking ctx.Err() check before select; route via PR (race path, both-platform CI).
- store Open/race-path fixes → always via PR, never direct-to-main.
