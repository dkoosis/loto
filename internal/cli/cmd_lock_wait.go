package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// loto-5gcy: a `loto lock` blocked ONLY by peer beacons waits for them to
// lapse instead of leaving the agent to write `sleep 20 && loto lock ...`.
//
// Why: 218 of 239 blocked lock calls over 14 days were blocked by a beacon —
// the 2-minute lease the PreToolUse hook mints on each write — not by a lock
// anyone asked for, and 46 of them already sat inside hand-written sleep
// loops the harness then refused.
//
// ‡ Capped. A beacon refreshes on every holder write (cmd_beacon.go
// beaconTTL), so "wait until it lapses" is unbounded while the peer keeps
// writing. The default cap sits under the Bash tool's 120s timeout; at the cap
// the refusal names the time to retry and exits exitLockBeaconWait, distinct
// from exit 1, so the agent can tell "a writer is busy, come back at T" from
// "someone holds this".
//
// ‡ Beacons only. A real lock is a declared intent with its own TTL and
// nothing tells us it lapses soon; it refuses at once with today's output. A
// batch with even one real-lock blocker refuses at once too — waiting out the
// beacons would only burn the cap before the same refusal.
//
// A lock is never granted over a live beacon: the store decides every
// attempt, and this loop only chooses when to ask again.

// defaultBeaconWait is the --wait default: under the Bash tool's 120s timeout
// with room for the store work around it.
const defaultBeaconWait = 90 * time.Second

// exitLockBeaconWait is `loto lock`'s exit code when peer beacons outlived
// the wait cap. 1 stays "blocked by a lock"; 2 usage; 3 IO.
const exitLockBeaconWait = 4

// beaconPollMax bounds one sleep so a beacon released or shortened early is
// noticed without waiting out the TTL it was minted with.
const beaconPollMax = 5 * time.Second

// beaconPollSlack lands the re-check just past a beacon's expiry — the store
// reads a row at its exact expires_at instant as still live.
const beaconPollSlack = 50 * time.Millisecond

// waitClock is the lock verb's clock for the beacon wait, a seam so tests
// need not sleep out a 90s cap. The store keeps reading real time.
type waitClock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realWaitClock struct{}

func (realWaitClock) Now() time.Time { return time.Now() }

func (realWaitClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var lockWaitClock waitClock = realWaitClock{}

// lockRequest carries the lock verb's flags into acquireBatch.
type lockRequest struct {
	intent string
	ttl    time.Duration
	mode   string
	wait   time.Duration
}

// beaconWaitCappedError is the refusal after the cap: the last conflict seen
// and the cap that was spent on it.
type beaconWaitCappedError struct {
	conflict *store.MultiConflictError
	wait     time.Duration
}

func (e *beaconWaitCappedError) Error() string {
	return fmt.Sprintf("peer beacon still live after wait=%s", e.wait)
}

// acquireWaitingOnBeacons tries the acquire, and while every blocker is a
// beacon and the cap has time left, sleeps to the earliest beacon expiry and
// tries again. It returns the records and the clock of the attempt that
// decided, or the store's error — a *beaconWaitCappedError when the cap ran
// out on beacons alone.
func acquireWaitingOnBeacons(rt *runtime, targets []domain.Target, req lockRequest, live domain.HolderLiveProbe, stderr io.Writer) ([]domain.LockRecord, time.Time, error) {
	deadline := lockWaitClock.Now().Add(req.wait)
	announced := false
	for {
		now := time.Now()
		recs := buildLockRecords(targets, rt, req.intent, now, req.ttl, req.mode)
		acquired, err := rt.Store.AcquireLocks(rt.Ctx, recs, live)
		mce, ok := errors.AsType[*store.MultiConflictError](err)
		if !ok || !allBeacons(mce.Blockers) {
			return acquired, now, err
		}
		d := nextBeaconPoll(mce.Blockers, lockWaitClock.Now(), deadline)
		if d <= 0 {
			return nil, now, &beaconWaitCappedError{conflict: mce, wait: req.wait}
		}
		if !announced {
			announced = true
			fmt.Fprintf(stderr, "ℹ waiting on peer beacon count=%d wait=%s\n", len(mce.Blockers), req.wait)
		}
		if serr := lockWaitClock.Sleep(rt.Ctx, d); serr != nil {
			return nil, now, serr
		}
	}
}

// allBeacons reports whether every blocker is a hook-minted beacon. Empty is
// false: no blockers is not a beacon block.
func allBeacons(blockers []domain.LockRecord) bool {
	if len(blockers) == 0 {
		return false
	}
	for i := range blockers {
		if !blockers[i].IsBeacon() {
			return false
		}
	}
	return true
}

// nextBeaconPoll is how long to sleep before the next attempt: to just past
// the earliest blocker's expiry, at most beaconPollMax, never past deadline.
// Zero or less means the cap is spent.
func nextBeaconPoll(blockers []domain.LockRecord, now, deadline time.Time) time.Duration {
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	d := beaconPollMax
	for i := range blockers {
		if until := blockers[i].ExpiresAt.Sub(now) + beaconPollSlack; until < d {
			d = until
		}
	}
	d = max(d, beaconPollSlack)
	return min(d, remaining)
}

// emitBeaconRetryAfter prints the line under the blocker rows that tells the
// agent when to come back.
func emitBeaconRetryAfter(w io.Writer, e *beaconWaitCappedError) {
	times := make([]time.Time, 0, len(e.conflict.Blockers))
	for i := range e.conflict.Blockers {
		times = append(times, e.conflict.Blockers[i].ExpiresAt)
	}
	fmt.Fprintf(w, "✗ peer beacon still live after wait=%s: retry after %s\n", e.wait, formatRetryAfter(times))
}

// formatRetryAfter is the latest of the expiries, rounded up to the second,
// as HH:MM:SS UTC — the first instant every blocker has lapsed, unless its
// holder writes again.
func formatRetryAfter(expiries []time.Time) string {
	var latest time.Time
	for _, t := range expiries {
		if t.After(latest) {
			latest = t
		}
	}
	latest = latest.UTC()
	if r := latest.Truncate(time.Second); r.Before(latest) {
		latest = r.Add(time.Second)
	}
	return latest.Format("15:04:05") + " UTC"
}
