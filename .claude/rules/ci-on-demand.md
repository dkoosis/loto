# CI On-Demand — opt expensive review in at PR time

*Codex (OpenAI spend) does not run on every PR. Claude opts a PR in with a comment when the diff warrants it. Default OFF; unsure → don't opt in.*

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

## macOS — every PR, locally

CI runs linux only (dk, 2026-10-07, #399). The local `make check` on dk's Mac, run before every ship, is the macOS proof. ✗ rent a macOS runner, ✗ ship without a local `make check`.
