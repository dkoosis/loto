package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatusCollisions pins loto-bo8c: two distinct agents shared-locking the
// same target coexist (Conflicts(shared,shared)=false), so `loto check` never
// surfaces them — but `status --collisions` flags the target as a ≥2-owner
// collision, the signal the shared beacon exists to expose.
func TestStatusCollisions(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	lockShared := func(uuid string) {
		t.Setenv("LOTO_AGENT_ID", uuid)
		if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, "--shared"},
			&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("shared lock for %s failed", uuid)
		}
	}
	lockShared(alice.UUID)
	lockShared(bob.UUID)

	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, "--collisions"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status --collisions exit: %q", out.String())
	}
	got := out.String()
	if !strings.Contains(got, "⚠ collisions count=1") {
		t.Errorf("expected 1 collision: %q", got)
	}
	if !strings.Contains(got, "target=a.go distinct_owners=2") {
		t.Errorf("expected a.go with 2 distinct owners: %q", got)
	}
	if !strings.Contains(got, alice.UUID) || !strings.Contains(got, bob.UUID) {
		t.Errorf("both colliding owners must be named: %q", got)
	}
}

// A single shared owner is not a collision — coexistence needs two DISTINCT
// owners on one target.
func TestStatusCollisions_NoneWhenSingleOwner(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, "--shared"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("shared lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, "--collisions"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ no collisions") {
		t.Errorf("single shared owner must not collide: %q", out.String())
	}
}

func TestStatusEmpty(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"project:", "repo:", "state:", "no locks"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in: %q", want, out.String())
		}
	}
}

func TestStatusMineFilters(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{"lock", tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdStatus, tcFlagMine}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "target=a.go") {
		t.Errorf("expected own lock listed: %q", out.String())
	}
}

func TestStatusSingleTargetFree(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdStatus, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "✓ free") {
		t.Errorf("expected ✓ free: %q", out.String())
	}
}

// TestStatusShowsTTLAndLiveness pins loto-k5el.1 SC3: status reports remaining
// TTL and an owner-liveness verdict per lock.
func TestStatusShowsTTLAndLiveness(t *testing.T) {
	withTempProject(t)
	// Lock with default TTL (30m) and no durable LOTO_PID → liveness UNKNOWN,
	// remaining TTL ~30m.
	t.Setenv("LOTO_PID", "")
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "ttl_remaining=") {
		t.Errorf("status must show ttl_remaining=: %q", s)
	}
	if !strings.Contains(s, "liveness=unknown") {
		t.Errorf("status must show liveness verdict (unknown for PID-0 sentinel): %q", s)
	}
}

// TestStatusDeadVerdictMatchesReclaim pins loto-k5el.1 I3: a lock status calls
// `dead` (expired TTL) is reclaimed by a peer acquire with no doctor run — so
// status's verdict is trustworthy, not cosmetic.
//
// Harness note (Task 0): two agents via re-pinning (no pinAgentAs).
func TestStatusDeadVerdictMatchesReclaim(t *testing.T) {
	withTempProject(t)
	t.Setenv("LOTO_PID", "") // PID-0 sentinel → TTL-only liveness
	pinAgent(t)              // agent A
	// D2 (loto-ebkc) rejects non-positive TTLs, so a born-stale fixture is no
	// longer expressible: take the shortest lease and wait out the expiry.
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	time.Sleep(20 * time.Millisecond) // lease expired → lock now stale
	var st bytes.Buffer
	Run([]string{tcCmdStatus}, &st, &bytes.Buffer{})
	if !strings.Contains(st.String(), "liveness=dead") && !strings.Contains(st.String(), "ttl_remaining=0s") {
		t.Fatalf("status should flag expired lock dead / 0s: %q", st.String())
	}
	pinAgent(t) // agent B (re-pin swaps active identity)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob should reclaim the dead-verdict lock with no doctor")
	}
}

// TestStatusDeadLockRendersDistinctFromLive pins sd-kck: a dead/expired lock
// row must not carry the same ✓ mark as a live one, the whole-repo summary
// must split live from expired, and the row must name the reclaim command —
// the defect was `loto status` printing ✓ on every row regardless of the
// liveness= it had already computed, so a pile of 49 expired locks read as
// "53 healthy".
func TestStatusDeadLockRendersDistinctFromLive(t *testing.T) {
	repo := withTempProject(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_PID", "") // PID-0 sentinel → TTL-only liveness
	pinAgent(t)              // agent A — dead lock
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	time.Sleep(20 * time.Millisecond) // lease expired → lock now dead
	pinAgent(t)                       // agent B — live lock, distinct target
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob lock failed")
	}

	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()

	if !strings.Contains(s, "⚠ locks count=2 held=1 expired=1") {
		t.Errorf("summary must split live vs expired counts: %q", s)
	}
	if !strings.Contains(s, "✗ target=a.go") {
		t.Errorf("dead row must carry a mark distinct from ✓: %q", s)
	}
	if strings.Contains(s, "✓ target=a.go") {
		t.Errorf("dead row must NOT render with the live ✓ mark: %q", s)
	}
	if !strings.Contains(s, "✓ target=b.go") {
		t.Errorf("live row keeps the ✓ mark: %q", s)
	}
	if !strings.Contains(s, "loto doctor --repair") {
		t.Errorf("status must name the reclaim command: %q", s)
	}
}

// TestStatusClaimsSection pins the loto-7af9 status surface: a claims section
// after locks — explicit "✓ no claims" empty header, live rows with prefix
// first, expired rows filtered, --mine honored.
func TestStatusClaimsSection(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ no claims") {
		t.Errorf("empty claims section needs explicit header: %q", out.String())
	}

	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("claim failed")
	}
	out.Reset()
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✓ claims count=1") || !strings.Contains(s, "prefix=internal/store") {
		t.Errorf("claims section missing live row: %q", s)
	}
	if !strings.Contains(s, "ttl_remaining=") {
		t.Errorf("claims row must show ttl_remaining: %q", s)
	}

	// --mine from a different agent filters the claim out.
	pinAgent(t)
	out.Reset()
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status --mine exit: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ no claims") {
		t.Errorf("--mine must filter another agent's claim: %q", out.String())
	}
}

// TestStatusClaimsFiltersExpired pins the TTL display rule: an expired claim
// row is filtered from status (staleness is display-time; the row itself dies
// lazily in a later overlapping acquire).
func TestStatusClaimsFiltersExpired(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("claim failed")
	}
	time.Sleep(60 * time.Millisecond)
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	if !strings.Contains(out.String(), "✓ no claims") {
		t.Errorf("expired claim must be filtered: %q", out.String())
	}
}

// loto-dvx: parity with check (loto-d3l). `loto status /abs/path` for a file
// inside the repo must work instead of failing canonicalization.
func TestStatus_AcceptsAbsolutePathInsideRepo(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	abs := filepath.Join(repo, tcTargetA)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdStatus, abs}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ free") {
		t.Errorf("expected ✓ free: %q", out.String())
	}
}
