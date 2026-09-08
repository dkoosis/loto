package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/identity"
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

// statusRowFor returns the status line naming target=<target>, so a test can
// assert on that one row without the self marker or fix block leaking a
// false pass/fail from an unrelated line elsewhere in the dump.
func statusRowFor(t *testing.T, out, target string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "target="+target+" ") {
			return line
		}
	}
	t.Fatalf("no status row for target=%s in: %q", target, out)
	return ""
}

// statusRowForOwner returns the status line naming owner=<uuid> — used for
// the same-target-two-owners case, where both rows share target= and only
// owner= tells them apart.
func statusRowForOwner(t *testing.T, out, uuid string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "owner="+uuid) {
			return line
		}
	}
	t.Fatalf("no status row for owner=%s in: %q", uuid, out)
	return ""
}

// TestStatusSelfMarker_MarksOwnRow_NotPeers pins ferret-m3t's core AC: the
// unfiltered dump marks the caller's own row and leaves a peer's unmarked, so
// a caller answers "is this mine?" from one `loto status` call — the read
// that failed the night this bead was filed.
func TestStatusSelfMarker_MarksOwnRow_NotPeers(t *testing.T) {
	repo := withTempProject(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob lock failed")
	}

	// Alice's own `loto status`: her row (a.go) is marked, bob's (b.go) is not.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	aRow, bRow := statusRowFor(t, s, tcTargetA), statusRowFor(t, s, tcTargetB)
	if !strings.Contains(aRow, "owner="+alice.UUID) {
		t.Fatalf("sanity: a.go row must show alice's uuid — the same one whoami reports: %q", aRow)
	}
	if !strings.Contains(aRow, "self=true") {
		t.Errorf("caller's own row must be marked self=true: %q", aRow)
	}
	if strings.Contains(bRow, "self=true") {
		t.Errorf("a peer's row must NOT be marked self=true: %q", bRow)
	}
}

// TestStatusSelfMarker_SameTargetTwoOwners pins the exact incident shape: two
// owners each hold a row on the SAME target (shared locks), and the marking
// flips depending on who is asking — never both, never neither.
func TestStatusSelfMarker_SameTargetTwoOwners(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	lockShared := func(uuid string) {
		t.Setenv("LOTO_AGENT_ID", uuid)
		if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagShared},
			&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("shared lock for %s failed", uuid)
		}
	}
	lockShared(alice.UUID)
	lockShared(bob.UUID)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	if !strings.Contains(statusRowForOwner(t, s, alice.UUID), "self=true") {
		t.Errorf("alice must see her own row on the shared target marked: %q", s)
	}
	if strings.Contains(statusRowForOwner(t, s, bob.UUID), "self=true") {
		t.Errorf("alice must NOT see bob's row on the same target marked: %q", s)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	out.Reset()
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s = out.String()
	if strings.Contains(statusRowForOwner(t, s, alice.UUID), "self=true") {
		t.Errorf("bob must NOT see alice's row marked: %q", s)
	}
	if !strings.Contains(statusRowForOwner(t, s, bob.UUID), "self=true") {
		t.Errorf("bob must see his own row on the shared target marked: %q", s)
	}
}

// TestStatusSelfMarker_MarksOwnBeaconRow pins the second uuid space (Rules):
// a beacon row carries a PER-AGENT uuid derived from (parent, LOTO_SUBAGENT_ID
// stamp), distinct from the parent SESSION uuid on exclusive locks/claims. The
// caller that wrote the beacon, asking `loto status` under the SAME stamp,
// must still see its own row marked — the marker has to hold in both spaces,
// not just the one the incident happened to hit.
func TestStatusSelfMarker_MarksOwnBeaconRow(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // parent identity every stamped sibling derives from
	t.Setenv("LOTO_SUBAGENT_ID", "sibling-1")

	derived, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if code := Run([]string{gateIntentBeacon, tcTargetA}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("beacon failed")
	}

	// Still stamped: resolveSubagent is deterministic over (parent, stamp), so
	// this status call resolves to the exact per-agent uuid the beacon row
	// carries.
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	row := statusRowFor(t, out.String(), tcTargetA)
	if !strings.Contains(row, "owner="+derived.UUID) {
		t.Fatalf("sanity: row owner must be the derived per-agent uuid: %q", row)
	}
	if !strings.Contains(row, `intent="beacon:`) {
		t.Fatalf("sanity: row must be the beacon row, not an exclusive lock: %q", row)
	}
	if !strings.Contains(row, "self=true") {
		t.Errorf("caller's own beacon row (per-agent uuid) must be marked self=true: %q", row)
	}
}

// TestStatusForeignRowFixBlock_PresentWhenPeerRowExists pins design.md's
// actionable-findings rule (ferret-m3t): a dump the caller cannot fully
// attribute to itself carries the inline fix block naming whoami/-mine/tag —
// the affordances that existed the whole incident night and nothing pointed
// at.
func TestStatusForeignRowFixBlock_PresentWhenPeerRowExists(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "rows above with no self=true belong to a peer") {
		t.Fatalf("expected the foreign-row fix block: %q", s)
	}
	for _, want := range []string{"loto whoami", "loto status -mine", "loto tag"} {
		if !strings.Contains(s, want) {
			t.Errorf("fix block must name %q: %q", want, s)
		}
	}
}

// TestStatusForeignRowFixBlock_AbsentWhenAllSelf is the negative case design.md
// requires alongside the positive one: a dump with nothing foreign in it
// prints no fix block — there is nothing to attribute.
func TestStatusForeignRowFixBlock_AbsentWhenAllSelf(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit: %q", out.String())
	}
	if strings.Contains(out.String(), "rows above with no self=true") {
		t.Errorf("a self-only dump must not print the foreign-row fix block: %q", out.String())
	}
}

// TestStatusMineAndCollisions_GoldenUnchanged pins the AC that this bead must
// not move `-mine`/`-collisions` output: neither the self marker nor the
// foreign-row fix block belong there — `-mine` already answers "is this
// mine?" by construction, and `-collisions` is its own, unrelated surface.
func TestStatusMineAndCollisions_GoldenUnchanged(t *testing.T) {
	repo := withTempProject(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("bob shared lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var mineOut bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &mineOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status --mine exit: %q", mineOut.String())
	}
	if strings.Contains(mineOut.String(), "self=true") {
		t.Errorf("-mine output must stay unchanged (no self marker): %q", mineOut.String())
	}
	if strings.Contains(mineOut.String(), "rows above with no self=true") {
		t.Errorf("-mine output must stay unchanged (no fix block): %q", mineOut.String())
	}

	var colOut bytes.Buffer
	if code := Run([]string{tcCmdStatus, "--collisions"}, &colOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status --collisions exit: %q", colOut.String())
	}
	if strings.Contains(colOut.String(), "self=true") || strings.Contains(colOut.String(), "rows above with no self=true") {
		t.Errorf("-collisions output must stay unchanged: %q", colOut.String())
	}
}
