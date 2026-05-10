# plan: loto-xra (gh #21)

## Direction

Tighten `try --hold` cleanup so that:
(a) panics or early returns don't leak the on-disk tag file
(b) double-SIGINT during Unlock can't trigger Go's default exit handler mid-cleanup
(c) signal handler is unregistered when the function returns

Three small asymmetries, one root cause: ad-hoc unlock placement + leaky signal handler. Fix the helper, fix the two callers identically.

## Files edited

| file | change |
|---|---|
| cmd/loto/main.go | fileCmd RunE: `defer lock.Unlock()`; same for globalCmd; waitForSignal: `defer signal.Stop(c)` |
| cmd/loto/main_test.go (or new file) | regression tests |

## Steps

1. **`waitForSignal`**: add `defer signal.Stop(c)` before `<-c`. Buffer stays 1 (enough — first SIGINT goes into chan and unblocks; we then proceed to defers).
2. **fileCmd RunE / globalCmd RunE**: replace trailing `_ = lock.Unlock()` with `defer func() { _ = lock.Unlock() }()` immediately after `acquireFile`/`acquireGlobal` succeeds. Removes both the panic-skip path (a) and gives a stable cleanup ordering.
3. **Defer ordering for (b)**: in `waitForSignal`, `defer signal.Stop(c)` runs *after* `<-c` returns, so a second SIGINT delivered while the caller's `defer Unlock` runs is still routed into the (already-drained) buffered chan rather than the default handler. Buffer-1 + still-installed Notify suppresses the second-signal exit. Verify by reading docs of `signal.Notify`: subsequent signals are dropped silently when chan is full or handler is registered — they do NOT fall back to default while Notify is live.
4. **Tests** (cmd/loto/try_hold_test.go, new file):
   - `TestTryHold_DeferUnlocksOnPanic`: spin a goroutine that acquires a file lock, panics during hold (post-emit), recovers in a deferred fn — assert `.tag` file removed and flock released by inspecting the file.
   - `TestWaitForSignal_StopsHandlerOnReturn`: invoke `waitForSignal` in a goroutine; send SIGINT (via `syscall.Kill(os.Getpid(), SIGINT)`); after it returns, send another SIGINT and verify the test process does not crash + no goroutine leak. Use `goleak`-style check or a sentinel.

   Pragmatic narrowing: if either test proves brittle in-process, narrow to: (a) source-level assertion via grep-in-test that `defer lock.Unlock()` and `defer signal.Stop(c)` exist (cheap regression guard); (b) keep behavioral test for the panic path which is reliable.

## Acceptance

- `defer lock.Unlock()` present after successful acquire in both RunE blocks
- `waitForSignal` has `defer signal.Stop(c)` (or equivalent via release fn)
- Tests cover: (a) Unlock-on-panic, (c) handler-stopped-after-return
- (b) double-SIGINT: documented via ordering of defers; if untestable in-process, leave as code comment + TODO bead reference (✗ untested-by-design avoided where possible)
- `make audit` green

## Out of scope

- `signal.NotifyContext` migration (deferrable)
- propagating ctx into Unlock (would change Locker API; separate bead)
- doctor's orphaned-tag handling (issue #19 territory)
