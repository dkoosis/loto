package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// TestStatusSingleTarget_NamesAClaimAbove: status on a file under a peer's
// claim reports the claim, as the gate that refuses the write does.
func TestStatusSingleTarget_NamesAClaimAbove(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("claim: exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t)
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcStoreStoreGo}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "✗ holder target="+tcPrefixStore+" ") {
		t.Errorf("want the claim above named: %q", out.String())
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
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "target="+target+" ") {
			return line
		}
	}
	t.Fatalf("no status row for target=%s in: %q", target, out)
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

// TestStatusSingleTarget_SelfMarkerOnHolderRows: `loto status <path>` is the
// read the PreToolUse gate makes to tell its own lock from a peer's on that
// path (loto-wuzh) — `✗ overlap` alone said nothing about whose, so the gate
// admitted a write over a peer's lock. The holder rows carry the same marker
// as the whole-repo view.
func TestStatusSingleTarget_SelfMarkerOnHolderRows(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	holderRow := func(asker string) string {
		t.Setenv("LOTO_AGENT_ID", asker)
		var out bytes.Buffer
		if code := Run([]string{tcCmdStatus, tcTargetA}, &out, &bytes.Buffer{}); code != 0 {
			t.Fatalf("status exit: %q", out.String())
		}
		for line := range strings.SplitSeq(out.String(), "\n") {
			if strings.HasPrefix(line, "✗ holder ") && strings.Contains(line, "owner="+alice.UUID+" ") {
				return line
			}
		}
		t.Fatalf("no holder row for alice: %q", out.String())
		return ""
	}
	if mine := holderRow(alice.UUID); !strings.HasSuffix(mine, " self=true") {
		t.Errorf("caller's own holder row must end self=true: %q", mine)
	}
	if peer := holderRow(bob.UUID); strings.Contains(peer, "self=true") {
		t.Errorf("a peer's holder row must be unmarked: %q", peer)
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

// TestStatusMine_GoldenUnchanged pins the AC that this bead must
// not move `-mine` output: neither the self marker nor the foreign-row fix
// block belong there — `-mine` already answers "is this mine?" by
// construction.
func TestStatusMine_GoldenUnchanged(t *testing.T) {
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
}
