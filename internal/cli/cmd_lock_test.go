package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
)

// initBareGitRepo runs `git init` and the minimal user.email/user.name config
// in `dir`. Shared by withTempProject and any test that builds an auxiliary
// repo (e.g., TestLoadCheckTargets_UsesRepoTopForGitDiff).
func initBareGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// withTempProject sets up a fresh git repo, a shared LOTO_BASE state dir, and a
// fresh HOME. Returns repoTop. Caller can use newAgent() to get a new identity.
func withTempProject(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	stateBase := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("LOTO_BASE", stateBase)
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	repo := t.TempDir()
	initBareGitRepo(t, repo)
	cmd := exec.Command("git", "remote", "add", "origin", "git@github.com:test/proj.git")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(repo)
	// Standard target files used across CLI tests. AcquireLocks Lstat-validates
	// KindFile targets, so these must exist on disk.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "store.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// pinAgent creates a fresh persisted identity in the current HOME and pins
// LOTO_AGENT_ID to it. Subsequent Run() calls will reuse this identity until
// LOTO_AGENT_ID is reset. Uses a unique CLAUDE_CODE_SESSION_ID to mint via the
// session-cache path so the identity is written to disk (loadByUUID can find
// it on subsequent Ensure calls).
func pinAgent(t *testing.T) *identity.Agent {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("pin-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", a.UUID)
	return a
}

var pinCounter atomic.Int64

// tcIntentAliceClaim is the shared claim intent string for loto-qoq's
// foreign-claim advisory tests (cmd_lock_test.go + cmd_check_test.go).
const tcIntentAliceClaim = "alice-claim"

// threeAgents extends twoAgents with a third identity — advisory tests
// (loto-qoq) need two independent foreign claimants distinct from the locker.
func threeAgents(t *testing.T) (alice, bob, carol *identity.Agent) {
	t.Helper()
	alice, bob = twoAgents(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("carol-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	c, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return alice, bob, c
}

func TestLockHappyPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, "10m", tcFlagIntent, tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected ✓ locked: %q", out.String())
	}
}

// twoAgents creates two agents in the shared HOME (simulating two sessions on
// the same host) and returns them.
func twoAgents(t *testing.T) (alice, bob *identity.Agent) {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("alice-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("bob-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	b, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestLockConflictBetweenAgents(t *testing.T) {
	withTempProject(t)
	alice := pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice initial lock failed, exit %d", code)
	}

	// Second agent in the same HOME.
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "✗ blocked") || !strings.Contains(combined, "blocker=") {
		t.Errorf("expected blocker report: %q", combined)
	}
	_ = alice
}

func TestLock_MultiTarget_HappyPath(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcTargetB, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "✓ locked count=2\n") {
		t.Errorf("missing triage line: %q", out.String())
	}
	for _, n := range []string{tcTargetA, tcTargetB} {
		if !strings.Contains(out.String(), n) {
			t.Errorf("%s missing from success rows: %q", n, out.String())
		}
	}
}

// TestLock_NonPositiveTTL_Rejected pins D2 (loto-ebkc): `lock --ttl 0` or a
// negative TTL used to be accepted, minting an instantly-stale lock any peer
// could reclaim the moment it was taken. Mirror the claim verb's guard
// (cmd_claim.go).
func TestLock_NonPositiveTTL_Rejected(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	for _, ttl := range []string{"0", "-5m"} {
		var out, errBuf bytes.Buffer
		code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, ttl, "-t", tcIntentTest}, &out, &errBuf)
		if code != 2 {
			t.Fatalf("--ttl %s: exit %d, want 2; out=%q err=%q", ttl, code, out.String(), errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "--ttl must be positive") {
			t.Errorf("--ttl %s: expected positivity diagnostic, got %q", ttl, errBuf.String())
		}
	}
	// No lock row landed and the target file stayed writable (the guard fires
	// before openRuntime, so no strip can have happened).
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if strings.Contains(out.String(), "target="+tcTargetA) {
		t.Errorf("no lock should exist after rejected ttl: %q", out.String())
	}
	if st, err := os.Stat(tcTargetA); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("rejected ttl must not strip the file, got %o", st.Mode().Perm())
	}
}

func TestCmdLock_SharedFlag_AllowsCoexist(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice shared lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("second shared lock should succeed; code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestCmdLock_DefaultExclusive_Blocks(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice exclusive lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("default exclusive should block; code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestLock_RejectDirectoryTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "internal/store", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "not-regular-file") {
		t.Errorf("expected reason=not-regular-file: %q", combined)
	}
}

func TestLock_RejectNonExistentTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "missing.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "not-found") {
		t.Errorf("expected reason=not-found: %q", combined)
	}
}

func TestLock_RejectsDuplicateTargets(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcTargetA, "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "duplicate-target") {
		t.Errorf("expected duplicate-target: %q", combined)
	}
}

func TestLock_RejectsSymlinks(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.Symlink(filepath.Join(repo, tcTargetA), filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "link.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "symlink") {
		t.Errorf("expected reason=symlink: %q", combined)
	}
}

// loto-dvx: parity with check (loto-d3l). `loto lock /abs/path` for a file
// inside the repo must succeed instead of being rejected as repo-escape.
func TestLock_AcceptsAbsolutePathInsideRepo(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	abs := filepath.Join(repo, tcTargetA)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, abs, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected ✓ locked: %q", out.String())
	}
}

func TestUnlockOwner(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, "-t", tcIntentDone}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("unlock exit %d; err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ unlocked") {
		t.Errorf("expected ✓ unlocked: %q", out.String())
	}
}

// TestAcquireReclaimsExpiredHolder_NoDoctor pins loto-k5el.1 SC1: after a
// holder's TTL has lapsed, a second agent acquires the same target with NO
// intervening `loto doctor`. Mechanism: AcquireLocks→reclaimStaleAndCollectBlockers.
//
// Not TDD — the acquire-time reclaim mechanism already ships; this test passes
// on first write and pins SC1 against regression.
//
// Harness note (Task 0): there is no pinAgentAs helper. Two agents are driven by
// re-pinning: pinAgent mints+pins a fresh identity, and re-pinning swaps the
// active LOTO_AGENT_ID (the same pattern TestLockConflictBetweenAgents uses).
func TestAcquireReclaimsExpiredHolder_NoDoctor(t *testing.T) {
	withTempProject(t)

	// Agent A locks with an already-expired TTL and the PID-0 sentinel (no
	// LOTO_PID), so liveness degrades to TTL and the lock is born stale.
	t.Setenv("LOTO_PID", "") // force pidUnset → PID-0 sentinel, TTL-only liveness
	pinAgent(t)              // agent A
	// D2 (loto-ebkc) rejects non-positive TTLs, so a born-stale fixture is no
	// longer expressible: take the shortest lease and wait out the expiry.
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice initial lock failed")
	}
	time.Sleep(20 * time.Millisecond) // lease expired → lock now stale

	// Agent B acquires the same target. No doctor run between.
	pinAgent(t) // agent B (re-pin swaps active identity)
	var out, errb bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &errb)
	if code != 0 {
		t.Fatalf("bob acquire over expired holder should succeed, got exit %d: out=%q err=%q",
			code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected success glyph in acquire output: %q", out.String())
	}
}

// --- loto-qoq: foreign-claim advisory on the lock success path ---

// TestLock_UnderForeignClaim_EmitsAdvisory pins the dominant case: locking a
// target under another agent's live claim succeeds (claims never block lock,
// cooperative model) and rides a ⚠ advisory after the success rows.
func TestLock_UnderForeignClaim_EmitsAdvisory(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentAliceClaim}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "✓ locked") {
		t.Errorf("expected success row: %q", s)
	}
	if !strings.Contains(s, "⚠ under-claim count=1") {
		t.Errorf("expected advisory header: %q", s)
	}
	if strings.Count(s, "under-claim owner=") != 1 {
		t.Errorf("expected exactly one advisory row: %q", s)
	}
	if !strings.Contains(s, "owner="+alice.UUID) || !strings.Contains(s, `intent="`+tcIntentAliceClaim+`"`) || !strings.Contains(s, "prefix="+tcPrefixStore) {
		t.Errorf("expected owner/intent/prefix on advisory row: %q", s)
	}
}

// TestLock_UnderOwnClaim_NoAdvisory: a claim the locker holds itself must not
// advise against itself.
func TestLock_UnderOwnClaim_NoAdvisory(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("claim failed")
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "under-claim") {
		t.Errorf("own claim must not emit advisory: %q", out.String())
	}
}

// TestLock_UnderExpiredClaim_NoAdvisory: a lapsed foreign claim must not
// advise.
func TestLock_UnderExpiredClaim_NoAdvisory(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}
	time.Sleep(20 * time.Millisecond)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if strings.Contains(out.String(), "under-claim") {
		t.Errorf("expired claim must not emit advisory: %q", out.String())
	}
}

// TestLock_NoClaims_OutputUnchanged pins the zero-claims case byte-identical
// to pre-change EmitLockSuccess output — no ⚠ block, no header.
func TestLock_NoClaims_OutputUnchanged(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	want := fmt.Sprintf("✓ locked count=1\n✓ target=%s mode=exclusive\n", tcTargetA)
	if out.String() != want {
		t.Errorf("output changed with zero claims:\ngot  %q\nwant %q", out.String(), want)
	}
}

// TestLock_TwoForeignClaims_SortedAdvisoryRows: one foreign owner (alice)
// holding claims at two ancestor prefixes of the same target — same-owner
// overlapping claims never conflict at claim time (unlike cross-owner
// overlap, which ClaimPrefix refuses), so this is the CLI-reachable shape of
// "two foreign claims covering one target" (mirrors gateDecide's own
// TestGateDecide_SameOwnerTwoPrefixes_BlockerPathTieBreak precedent). Owner
// is equal on both rows, so this exercises the Prefix tie-break: "internal"
// sorts before "internal/store".
func TestLock_TwoForeignClaims_SortedAdvisoryRows(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", "deep-claim"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice deep claim failed")
	}
	if code := Run([]string{tcCmdClaim, "internal", "-t", "shallow-claim"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice shallow claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdLock, tcStoreStoreGo, "-t", tcIntentTest}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "⚠ under-claim count=2") {
		t.Errorf("expected count=2: %q", s)
	}
	shallowIdx := strings.Index(s, "prefix=internal\n")
	deepIdx := strings.Index(s, "prefix="+tcPrefixStore+"\n")
	if shallowIdx == -1 || deepIdx == -1 || shallowIdx > deepIdx {
		t.Errorf("expected prefix-sorted advisory rows (internal before internal/store): %q", s)
	}
}

// TestCollectForeignClaimAdvisories_OwnAndForeignClaim_OnlyForeignEmits: a
// target under both an own claim and a foreign claim must advise on the
// foreign row only. Exercised directly against the pure collector (not the
// CLI): two DIFFERENT-owner claims covering the same target both stay live
// simultaneously only in a fixture — ClaimPrefix would refuse a second
// cross-owner claim overlapping a still-live foreign one, so this state
// isn't producible through the lock CLI (P2 boundary, plan test 12).
func TestCollectForeignClaimAdvisories_OwnAndForeignClaim_OnlyForeignEmits(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcStoreStoreGo}
	const meUUID = "aaaaaaaa-0000-0000-0000-000000000000"
	const foeUUID = "bbbbbbbb-0000-0000-0000-000000000000"
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: meUUID, Intent: "own-intent", ExpiresAt: now.Add(time.Hour)},
		{PathPrefix: "internal", OwnerUUID: foeUUID, Intent: "foreign-intent", ExpiresAt: now.Add(time.Hour)},
	}
	rows := collectForeignClaimAdvisories([]domain.Target{target}, claims, meUUID, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row (own claim filtered out), got %d: %+v", len(rows), rows)
	}
	if rows[0].Owner != foeUUID {
		t.Errorf("expected foreign row only: %+v", rows[0])
	}
}

// TestCollectForeignClaimAdvisories_DedupsDuplicateTarget: loto lock's own
// validateLockTargets already rejects a duplicate CLI arg as invalid (exit 2)
// before acquireBatch runs, so "loto lock foo foo" never reaches the
// advisory collector through the CLI. Exercise the seen-map dedup guard
// directly against the pure collector instead — mirrors how gateDecide's own
// dedup tests call the pure function with duplicate input.
func TestCollectForeignClaimAdvisories_DedupsDuplicateTarget(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcStoreStoreGo}
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: "foe-uuid", Intent: "foe", ExpiresAt: now.Add(time.Hour)},
	}
	rows := collectForeignClaimAdvisories([]domain.Target{target, target}, claims, "my-uuid", now)
	if len(rows) != 1 {
		t.Fatalf("want 1 deduped row for duplicate target input, got %d: %+v", len(rows), rows)
	}
}

// TestCollectForeignClaimAdvisories_OwnerTieBreak pins the (Target, Owner,
// Prefix) sort's Owner tie-break, which the bead's byte-identical-output
// acceptance criterion depends on. The store rejects two cross-owner claims
// over one live target, so this shape is CLI-unreachable — exercise it
// directly against the pure collector (parallel to the dedup/own+foreign
// tests above), feeding the two owners in reverse order to prove the sort,
// not the input order, decides the row order.
func TestCollectForeignClaimAdvisories_OwnerTieBreak(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcStoreStoreGo}
	const ownerA = "aaaaaaaa-0000-0000-0000-000000000000"
	const ownerB = "bbbbbbbb-0000-0000-0000-000000000000"
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: ownerB, Intent: "b", ExpiresAt: now.Add(time.Hour)},
		{PathPrefix: tcPrefixStore, OwnerUUID: ownerA, Intent: "a", ExpiresAt: now.Add(time.Hour)},
	}
	rows := collectForeignClaimAdvisories([]domain.Target{target}, claims, "my-uuid", now)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Owner != ownerA || rows[1].Owner != ownerB {
		t.Errorf("want Owner-sorted A before B, got %s then %s", rows[0].Owner, rows[1].Owner)
	}
}

// TestLock_StampsHolderBranch pins loto-16cf: acquire stamps the holder's
// current git branch onto the lock record and status surfaces it as branch=;
// a detached HEAD records the short SHA instead. Pre-16cf rows (Branch=="")
// keep rendering without a branch= key — covered by every other test in this
// file, whose repos have no commits and so stamp "".
func TestLock_StampsHolderBranch(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "base")
	gitIn("checkout", "-q", "-b", "feature/16cf")

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock: exit %d", code)
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(out.String(), " branch=feature/16cf") {
		t.Errorf("status missing branch=feature/16cf:\n%s", out.String())
	}

	gitIn("checkout", "-q", "--detach")
	short := gitIn("rev-parse", "--short", "HEAD")
	if code := Run([]string{tcCmdLock, filepath.Join("internal", "store", "store.go"), "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock detached: exit %d", code)
	}
	out.Reset()
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(out.String(), " branch="+short) {
		t.Errorf("status missing branch=%s for detached HEAD:\n%s", short, out.String())
	}
}

// TestLock_SymlinkedAncestorCannotDoubleHold is the loto-j39r P1, end to end:
// two agents locking one file through two directory spellings. Before ancestor
// resolution both acquires succeeded, and each agent believed it held the file
// exclusively.
func TestLock_SymlinkedAncestorCannotDoubleHold(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	realDir := filepath.Join(repo, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, "real/" + tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock via the real path failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdLock, "link/" + tcTargetA, "-t", tcIntentTest}, &out, &bytes.Buffer{}); code == 0 {
		t.Fatalf("bob acquired the same file through a symlinked ancestor: %q", out.String())
	}
}

// TestLock_SymlinkAliasesDedupeWithinOneBatch pins the other half: once the two
// spellings converge they are one target, so a single batch naming both is a
// duplicate rather than two locks.
func TestLock_SymlinkAliasesDedupeWithinOneBatch(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	realDir := filepath.Join(repo, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "real/" + tcTargetA, "link/" + tcTargetA, "-t", tcIntentTest}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("expected the alias to be rejected as a duplicate: out=%q", out.String())
	}
	if !strings.Contains(errBuf.String()+out.String(), "duplicate-target") {
		t.Errorf("expected duplicate-target: out=%q err=%q", out.String(), errBuf.String())
	}
}
