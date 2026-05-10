# Boot
updated: 2026-05-09

→ `bd ready`. Empty → review backlog with dk.

✓ done
- magloop sweep: goconst×33 + modernize + nolintlint extracted to consts (b57d86a, unpushed)
- removed stale `loto-ux3.1/` worktree that was poisoning lint cache

‡ traps
- `make audit` may be lying about lint state if golangci-lint cache is poisoned by a sibling worktree of the same module — clear with `golangci-lint cache clean` then `rm -rf ~/Library/Caches/golangci-lint/*`
