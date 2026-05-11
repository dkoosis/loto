# Boot
updated: 2026-05-10 (Sun late)

→ ship loto coordination primitives: `bd show loto-nbl loto-xfx loto-036`. Pick one, plan, TDD.

state: qqh.2 staged in `john-loto-qqh.2/` on branch `loto-qqh.2` — Task 11 (cmd_lock.go) bundles the commit.

✓ done
- loto-qqh.2 staged (AcquireLocks + 4 tests + ports)
- 3-agent session: PRs #59 #60 #61 + filed loto-w0s family + loto-1w5

‡ traps
- `loto inbox` silently advances cursor → `--unread` polling unreliable until loto-036.
