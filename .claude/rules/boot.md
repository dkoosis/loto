# Boot
updated: 2026-05-17

→ extend testscript coverage — doctor stale-break, hook entrypoints, error paths. `go test ./cmd/loto/ -run TestScripts`

✓ done
- testscript wired (4 scripts pass) + LOTO_PID hook for subprocess-stamped locks

‡ traps
- per-script setup mints agents directly to disk (✗ os.Setenv races); see mintAgentFile
