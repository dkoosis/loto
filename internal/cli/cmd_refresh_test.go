package cli

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAcceptance_ExpiredLeaseFreesTerritoryWithoutDoctor is ccp-z1vj.6's
// expiry half: alice locks with a short TTL and never comes back (crashed
// agent). Bob acquires the same target after the lease lapses — no doctor run
// anywhere in the flow. Alice's PID is this live test process on purpose: the
// TTL alone must free the territory, not the dead-pid probe.
func TestAcceptance_ExpiredLeaseFreesTerritoryWithoutDoctor(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	// Lease measured from here: the pre-expiry block check below must land
	// inside it, and each Run opens+migrates the store. The margin is sized for
	// a loaded serial CI runner; the sleep below runs to the deadline, so a
	// wider margin costs wall time only when the Runs finish fast.
	const ttl = 10 * time.Second
	deadline := time.Now().Add(ttl)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "crash me", tcFlagTTL, ttl.String()}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	// Peer is blocked while the lease is live.
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentWrite}, &out, io.Discard); code != 1 {
		t.Fatalf("bob must be blocked pre-expiry, got %d: %s", code, out.String())
	}

	time.Sleep(time.Until(deadline) + 200*time.Millisecond) // outlive alice's lease

	out.Reset()
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentWrite}, &out, io.Discard); code != 0 {
		t.Fatalf("bob must acquire after expiry without doctor, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ locked count=1") {
		t.Errorf("expected acquire, got: %s", out.String())
	}
}

// TestAcceptance_RefreshBeforeExpiryHoldsTerritory is the refresh half: a live
// holder extends its lease past the original deadline, and the peer stays
// blocked after the original TTL would have lapsed.
func TestAcceptance_RefreshBeforeExpiryHoldsTerritory(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	// The original lease must outlive the refresh invocation itself (each Run
	// opens+migrates the store), so it is measured from here rather than assumed,
	// with the same loaded-CI margin as the expiry test above.
	const origTTL = 10 * time.Second
	deadline := time.Now().Add(origTTL)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "long haul", tcFlagTTL, origTTL.String()}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdRefresh, tcTargetA, tcFlagTTL, "1h"}, &out, io.Discard); code != 0 {
		t.Fatalf("refresh exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ refreshed count=1") {
		t.Errorf("expected refresh confirmation, got: %s", out.String())
	}

	time.Sleep(time.Until(deadline) + 200*time.Millisecond) // past the ORIGINAL lease, inside the refreshed one

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	out.Reset()
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentWrite}, &out, io.Discard); code != 1 {
		t.Fatalf("refreshed lease must still block bob, got %d: %s", code, out.String())
	}
}

// TestRefresh_NotHeldReportsAndExits1 — refreshing territory you don't hold is
// a per-target ✗ with exit 1, not a silent success.
func TestRefresh_NotHeldReportsAndExits1(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdRefresh, tcTargetA}, &out, io.Discard)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "no-lock-held") {
		t.Errorf("expected no-lock-held reason, got: %s", out.String())
	}
}

// TestRefresh_All refreshes every lock this agent holds — the heartbeat shape a
// long-running agent uses instead of naming each target.
func TestRefresh_All(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	for _, tgt := range []string{tcTargetA, tcStoreStoreGo} {
		if code := Run([]string{tcCmdLock, tgt, tcFlagIntent, tcIntentWrite, tcFlagTTL, "5m"}, io.Discard, io.Discard); code != 0 {
			t.Fatalf("lock %s failed", tgt)
		}
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdRefresh, tcFlagAll, tcFlagTTL, "2h"}, &out, io.Discard); code != 0 {
		t.Fatalf("refresh --all exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ refreshed count=2") {
		t.Errorf("expected both locks refreshed, got: %s", out.String())
	}
}

// TestRefresh_AllLapsedLeaseIsAdvisoryNotFailure — a heartbeat that extends
// every live lease it owns must exit 0 even when some unrelated lock lapsed
// earlier: --all names no target, so the lapsed row is a ⚠ advisory, not the
// answer to a question the caller asked.
func TestRefresh_AllLapsedLeaseIsAdvisoryNotFailure(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentWrite, tcFlagTTL, "5m"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("live lock failed")
	}
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, tcIntentWrite, tcFlagTTL, "150ms"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("short-TTL lock failed")
	}
	time.Sleep(250 * time.Millisecond) // let the second lease lapse

	var out bytes.Buffer
	if code := Run([]string{tcCmdRefresh, tcFlagAll, tcFlagTTL, "2h"}, &out, io.Discard); code != 0 {
		t.Fatalf("--all must not fail on a lapsed lease, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ refreshed count=1") {
		t.Errorf("expected the live lease refreshed, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "⚠") || !strings.Contains(out.String(), "lease-expired") {
		t.Errorf("expected a ⚠ lease-expired advisory row, got: %s", out.String())
	}
}

// TestRefresh_ExplicitLapsedLeaseExits1 — the same lapsed lease named
// explicitly is a ✗ with exit 1: the caller asked about that lock.
func TestRefresh_ExplicitLapsedLeaseExits1(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentWrite, tcFlagTTL, "150ms"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("short-TTL lock failed")
	}
	time.Sleep(250 * time.Millisecond)

	var out bytes.Buffer
	if code := Run([]string{tcCmdRefresh, tcTargetA, tcFlagTTL, "2h"}, &out, io.Discard); code != 1 {
		t.Fatalf("want exit 1 for an explicitly named lapsed lease, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗") || !strings.Contains(out.String(), "lease-expired") {
		t.Errorf("expected a ✗ lease-expired row, got: %s", out.String())
	}
}

// TestRefresh_RejectsNonPositiveTTL mirrors lock/claim: a non-positive TTL
// would mint an instantly-reclaimable lease.
func TestRefresh_RejectsNonPositiveTTL(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var errBuf bytes.Buffer
	if code := Run([]string{tcCmdRefresh, tcTargetA, tcFlagTTL, "0s"}, io.Discard, &errBuf); code != 2 {
		t.Fatalf("want exit 2 for --ttl 0s, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "--ttl must be positive") {
		t.Errorf("expected positive-TTL rejection, got: %s", errBuf.String())
	}
}
