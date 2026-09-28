package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

// loto-5gcy: a `loto lock` blocked only by a peer's BEACON waits for it to
// lapse, capped (default 90s, --wait overrides), then refuses with
// "retry after HH:MM:SS" and exitLockBeaconWait. A real lock refuses at once
// with today's output.

const tcFlagWait = "--wait"

// fakeWaitClock advances a virtual clock on Sleep instead of sleeping, so the
// cap test runs in milliseconds. The store still reads real time, which is why
// the cap test keeps its beacon alive for minutes of real time.
type fakeWaitClock struct {
	now    time.Time
	slept  time.Duration
	sleeps int
}

func (c *fakeWaitClock) Now() time.Time { return c.now }

func (c *fakeWaitClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	c.slept += d
	c.sleeps++
	return nil
}

// failOnSleepClock fails the test if the lock verb waits at all.
type failOnSleepClock struct{ t *testing.T }

func (c failOnSleepClock) Now() time.Time { return time.Now() }

func (c failOnSleepClock) Sleep(context.Context, time.Duration) error {
	c.t.Helper()
	c.t.Error("lock waited, want an immediate refusal")
	return nil
}

func withLockWaitClock(t *testing.T, c waitClock) {
	t.Helper()
	prev := lockWaitClock
	lockWaitClock = c
	t.Cleanup(func() { lockWaitClock = prev })
}

// AC 1: while A's beacon lives, B's lock does not succeed — it waits up to
// the cap, then refuses with the retry-after time and the distinct exit code.
func TestLock_PeerBeaconWaitsToCapThenRefuses(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdBeacon, tcTargetA}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice beacon failed, exit %d", code)
	}

	clk := &fakeWaitClock{now: time.Now()}
	withLockWaitClock(t, clk)
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &out, &errBuf)
	if code != exitLockBeaconWait {
		t.Fatalf("exit=%d, want %d; out=%q err=%q", code, exitLockBeaconWait, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "✓ locked") {
		t.Fatalf("lock granted over a live peer beacon: %q", out.String())
	}
	if clk.sleeps == 0 {
		t.Error("lock refused without waiting on a beacon-only block")
	}
	if clk.slept > defaultBeaconWait {
		t.Errorf("waited %s, want at most the %s cap", clk.slept, defaultBeaconWait)
	}
	if clk.slept < defaultBeaconWait {
		t.Errorf("waited %s, want the full %s cap before refusing", clk.slept, defaultBeaconWait)
	}
	if !strings.Contains(out.String(), "✗ blocked count=1") {
		t.Errorf("want the blocker block on refusal: %q", out.String())
	}
	if !strings.Contains(out.String(), "retry after ") {
		t.Errorf("want 'retry after HH:MM:SS' on refusal: %q", out.String())
	}
}

// --wait overrides the cap; --wait 0 refuses at once with the retry-after line.
func TestLock_PeerBeaconWaitFlag(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdBeacon, tcTargetA}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice beacon failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	clk := &fakeWaitClock{now: time.Now()}
	withLockWaitClock(t, clk)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite, tcFlagWait, "10s"}, &out, &bytes.Buffer{}); code != exitLockBeaconWait {
		t.Fatalf("--wait 10s: exit=%d, want %d; out=%q", code, exitLockBeaconWait, out.String())
	}
	if clk.slept != 10*time.Second {
		t.Errorf("--wait 10s: waited %s, want 10s", clk.slept)
	}

	withLockWaitClock(t, failOnSleepClock{t})
	out.Reset()
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite, tcFlagWait, "0"}, &out, &bytes.Buffer{}); code != exitLockBeaconWait {
		t.Fatalf("--wait 0: exit=%d, want %d; out=%q", code, exitLockBeaconWait, out.String())
	}
	if !strings.Contains(out.String(), "retry after ") {
		t.Errorf("--wait 0: want 'retry after': %q", out.String())
	}

	var errBuf bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite, tcFlagWait, "-1s"}, &bytes.Buffer{}, &errBuf); code != 2 {
		t.Fatalf("--wait -1s: exit=%d, want 2 (usage); err=%q", code, errBuf.String())
	}
}

// AC 2: once the beacon lapses, B gets the lock from the one call — no loop
// written by the agent. Real clock: the beacon's TTL is a few hundred ms.
func TestLock_PeerBeaconLapsesThenGranted(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdBeacon, tcTargetA, tcFlagTTL, "300ms"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice beacon failed, exit %d", code)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	start := time.Now()
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit=%d after the beacon lapsed; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ locked count=1") {
		t.Errorf("want ✓ locked: %q", out.String())
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("waited %s for a 300ms beacon", time.Since(start))
	}
	if !strings.Contains(errBuf.String(), "waiting") {
		t.Errorf("want a waiting notice on stderr: %q", errBuf.String())
	}
}

// AC 3: a real (non-beacon) lock refuses at once, with today's output and
// exit 1 — no wait, no retry-after line.
func TestLock_RealLockRefusesAtOnce(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed, exit %d", code)
	}

	withLockWaitClock(t, failOnSleepClock{t})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "✗ blocked count=1\n") {
		t.Errorf("want today's blocker block: %q", out.String())
	}
	if strings.Contains(out.String(), "retry after") || strings.Contains(errBuf.String(), "waiting") {
		t.Errorf("a real lock must not print the beacon-wait lines: out=%q err=%q", out.String(), errBuf.String())
	}
}

// A batch blocked by a beacon AND a real lock refuses at once: the real lock
// will not lapse on a beacon's schedule, so waiting would only burn the cap.
func TestLock_MixedBeaconAndRealLockRefusesAtOnce(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed, exit %d", code)
	}
	if code := Run([]string{tcCmdBeacon, tcStoreStoreGo}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice beacon failed, exit %d", code)
	}

	withLockWaitClock(t, failOnSleepClock{t})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, tcStoreStoreGo, "-t", tcIntentWrite}, &out, &bytes.Buffer{}); code != 1 {
		t.Fatalf("exit=%d, want 1; out=%q", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "✗ blocked count=2\n") {
		t.Errorf("want both blockers: %q", out.String())
	}
}

// formatRetryAfter is the HH:MM:SS the refusal names: the LAST blocker's
// expiry, in UTC, so every blocker has lapsed by then.
func TestFormatRetryAfter(t *testing.T) {
	loc := time.FixedZone("X", 5*3600)
	got := formatRetryAfter([]time.Time{
		time.Date(2026, 9, 28, 17, 0, 5, 0, loc),
		time.Date(2026, 9, 28, 12, 1, 7, 400e6, time.UTC),
	})
	if got != "12:01:08 UTC" {
		t.Errorf("formatRetryAfter = %q, want %q (latest, UTC, rounded up)", got, "12:01:08 UTC")
	}
}

// nextBeaconPoll never sleeps past the cap, never busy-loops on a beacon at
// or past its expiry, and wakes just past the earliest expiry.
func TestNextBeaconPoll(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) []domain.LockRecord {
		return []domain.LockRecord{{ExpiresAt: now.Add(d)}, {ExpiresAt: now.Add(time.Hour)}}
	}
	cases := []struct {
		name     string
		blockers []domain.LockRecord
		deadline time.Time
		want     time.Duration
	}{
		{"cap spent", at(time.Second), now, 0},
		{"cap past", at(time.Second), now.Add(-time.Second), 0},
		{"earliest expiry plus slack", at(time.Second), now.Add(time.Minute), time.Second + beaconPollSlack},
		{"far expiry polls at the max", at(time.Minute), now.Add(time.Minute), beaconPollMax},
		{"expired row floors at slack", at(-time.Second), now.Add(time.Minute), beaconPollSlack},
		{"never past the cap", at(time.Minute), now.Add(time.Millisecond), time.Millisecond},
	}
	for _, tc := range cases {
		if got := nextBeaconPoll(tc.blockers, now, tc.deadline); got != tc.want {
			t.Errorf("%s: nextBeaconPoll = %s, want %s", tc.name, got, tc.want)
		}
	}
}
