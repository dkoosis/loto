# CI On-Demand — opt expensive CI in at PR time

*Two costly CI paths no longer run on every PR. Claude opts a PR in when the diff warrants it — Codex (OpenAI spend) via a comment, the macOS leg (10× runner minutes) via a label. Default OFF for both; unsure → don't opt in.*

## Codex review — comment `@codex review`

Advisory; does **not** gate merge (`CI` is the gate).

```bash
gh pr comment <PR#> --body "@codex review"
```

| Request when diff has | Skip for |
|---|---|
| new/changed logic w/ branching or edge cases | docs / config / test-only |
| concurrency / lifecycle / goroutine code | mechanical renames/moves, `s/this/that/g` sweeps |
| store Open / race-path / persistence code | dependency bumps |
| identity registry / lock-coordination changes | one-liners |
| security-adjacent (auth, input handling) | generated code |
| large / sprawling change | reverts / cherry-picks of already-reviewed work |

## macOS leg — label `ci:macos` (HIGH BAR)

macOS leg of `CI` costs 10×. Runs weekly as a backstop; dk develops on macOS locally → routine darwin coverage already exists. **Label only when BOTH hold:**

1. Diff changes genuinely **OS-divergent** behavior — FS case-sensitivity, file mode/perm bits, path-separator handling, process/exec/signal, file locking, `//go:build darwin`/`!linux` code. ✗ "ordinary Go that runs on a Mac" (everything does).
2. dk **won't** run it on his Mac before merge (e.g. agent-authored PR merging without his local pass).

```bash
gh pr edit <PR#> --add-label ci:macos
```

‡ Default **do not label**. Doubt → skip; the weekly darwin backstop catches it. Bar is deliberately high — most PRs never need this.
