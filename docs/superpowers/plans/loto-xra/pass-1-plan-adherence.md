# pass-1 plan-adherence review — loto-xra (gh #21)

reviewed: 2026-05-10
scope: plan.md adherence + correctness of diff + test quality
reviewer: feature-dev:code-reviewer

## Plan delivery

| plan requirement | delivered |
|---|---|
| fileCmd RunE: `defer lock.Unlock()` | ✓ main.go:177 |
| globalCmd RunE: `defer lock.Unlock()` | ✓ main.go:198 |
| waitForSignal: `defer signal.Stop(c)` | ✓ main.go:895 |
| regression tests (new file) | ✓ cmd/loto/try_hold_test.go |
| out-of-scope files untouched | ✓ no other files modified |

All three plan §Steps addressed. No files outside the §Files edited table.

## Bug aspects

**(a) panic-skip orphan tag** — resolved. `defer func() { _ = lock.Unlock() }()` registers before `emitTrySuccess` and `waitForSignal`, so any panic unwinds through the defer.

**(b) double-SIGINT during Unlock** — resolved by ordering. `defer signal.Stop(c)` runs *after* `<-c` returns. During the caller's deferred Unlock, Notify is still live and the buffer-1 chan silently absorbs a second SIGINT rather than escalating to Go's default handler. Matches plan §Step 3.

**(c) signal handler leak** — resolved. `defer signal.Stop(c)` deregisters on every return path.

## Findings

**P1 — test false-pass risk on defer removal**
`cmd/loto/try_hold_test.go:73` · `TestTryRunE_DefersUnlock`
Regex `defer[^\n]*Unlock\(` matches any line containing those tokens, including comments. If the real defer is deleted but a nearby comment contains `Unlock(`, the test passes while the bug is live.
Fix: scope grep to just the RunE closure body using the existing brace-walker, or tighten pattern to `defer\s+func\(\)`.

**P2 — item (b) lacks code comment**
`cmd/loto/main.go:892` · plan §Acceptance says: "leave as code comment + TODO bead reference" if untestable. No comment in `waitForSignal` explains why defer ordering suppresses double-SIGINT.
Fix: one-line comment citing buffer-1 + still-live Notify absorbs second signal.

**P3 — behavioral panic-unwind test absent**
plan §Steps §4 explicitly permits source-grep narrowing when in-process is brittle. Acceptable per plan; no action this bead.

## Error-flow semantics

`exit(err)` calls `os.Exit` and never returns, so the defer only registers when acquire succeeded and `lock` is non-nil. No nil-deref risk. Defer does not change error-path behavior.

## North-star recenter

> Does this change move loto closer to its north star (transparent multi-agent coordination via filesystem locks + tag files), or sideways? One sentence.

Directly closer: sealing the unlock-on-panic and signal-leak gaps means tag files are reliably removed when `--hold` sessions end, which is the core promise of the coordination protocol.
