package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

const tcFlagGate = "--gate"

// gateDecide unit tests (loto-vr2, gate-design.md component 4). Pure —
// domain.LockRecord/domain.ClaimRecord fixtures in, []gateDeny out — no
// store, no CLI, no clock read beyond the EvalContext/now passed in. These
// pin the deliberate divergences from plain check's computeCheckConflicts:
// any-mode foreign-lock deny, claims consulted at all, and !IsStale (not
// Classify==Alive) as the liveness threshold.

const (
	gateMyUUID  = "11111111-1111-1111-1111-111111111111"
	gateFoeUUID = "22222222-2222-2222-2222-222222222222"
)

func gateEC(now time.Time) domain.EvalContext {
	return domain.EvalContext{Now: now, ThisHost: "host-a", Live: nil}
}

func TestGateDecide_ForeignLiveClaimDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"} // kuv.10 class: not on disk
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "refactor", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 1 {
		t.Fatalf("want 1 deny row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Kind != gateKindClaim || rows[0].HolderUUID != gateFoeUUID || rows[0].Path != target.Canonical {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}

func TestGateDecide_OwnClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateMyUUID, Intent: "mine", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own claim must not deny, got %+v", rows)
	}
}

func TestGateDecide_ExpiredClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "internal/store/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "stale", ExpiresAt: now.Add(-time.Minute)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("expired claim must not deny, got %+v", rows)
	}
}

func TestGateDecide_ForeignSharedBeaconDenies(t *testing.T) {
	// Any-mode divergence from plain check: a foreign SHARED lock still denies
	// under the gate (plan point 1) — plain check's shared-vs-shared probe
	// would pass this.
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, Intent: "beacon", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].Kind != gateKindLock || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("want 1 lock-kind deny row, got %+v", rows)
	}
}

func TestGateDecide_ForeignStaleLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, Intent: "old", ExpiresAt: now.Add(-time.Minute)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("stale (TTL-lapsed) lock must not deny, got %+v", rows)
	}
}

func TestGateDecide_OwnLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, Mode: domain.ModeExclusive, Intent: "mine", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own lock must not deny, got %+v", rows)
	}
}

// TestGateDecide_PID0ForeignBeaconWithinTTLDenies pins the liveness-threshold
// divergence (plan point 3): the gate uses !IsStale, not Classify==Alive, so
// a hook-minted beacon with no durable PID (the sentinel PID<=0 — no
// LOTO_PID) still denies while its TTL hasn't lapsed. Classify would report
// this holder LivenessUnknown; plain check's Classify==Alive gate would only
// warn, never hard-block, on this row.
func TestGateDecide_PID0ForeignBeaconWithinTTLDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: "a.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, PID: 0, Intent: "beacon", ExpiresAt: now.Add(time.Hour)},
	}
	ec := gateEC(now)
	if ec.Classify(locks[0]) != domain.LivenessUnknown {
		t.Fatalf("test fixture invariant broken: want LivenessUnknown, got %v", ec.Classify(locks[0]))
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, ec)
	if len(rows) != 1 || rows[0].Kind != gateKindLock {
		t.Fatalf("PID-0 beacon within TTL must deny, got %+v", rows)
	}
}

// TestGateDecide_DedupeAndOrder: a claim AND a lock both cover the same
// path from the same foreign owner must not collapse into one row (distinct
// kinds), a duplicate target in the input must not double-count, and output
// is sorted path -> kind -> holder UUID regardless of input order.
func TestGateDecide_DedupeAndOrder(t *testing.T) {
	now := time.Now()
	tB := domain.Target{Canonical: "b.go"}
	tA := domain.Target{Canonical: "internal/store/a.go"}
	locks := []domain.LockRecord{
		{Target: tB, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, Intent: "lockrow", ExpiresAt: now.Add(time.Hour)},
	}
	claims := []domain.ClaimRecord{
		{PathPrefix: "internal/store", OwnerUUID: gateFoeUUID, Intent: "claimrow", ExpiresAt: now.Add(time.Hour)},
	}
	// tB passed twice (duplicate CLI arg) + tA once, out of sorted order.
	rows := gateDecide([]domain.Target{tB, tA, tB}, locks, claims, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 deduped rows (one per distinct path), got %d: %+v", len(rows), rows)
	}
	// path "b.go" < "internal/store/a.go" is false lexicographically ('b' >
	// 'i'? no: 'b'=0x62, 'i'=0x69, so "b.go" < "internal/store/a.go" is true).
	if rows[0].Path != "b.go" || rows[1].Path != "internal/store/a.go" {
		t.Fatalf("want path-sorted rows, got %+v", rows)
	}
	if rows[0].Kind != gateKindLock || rows[1].Kind != gateKindClaim {
		t.Fatalf("unexpected kinds: %+v", rows)
	}
}

// --- CLI acceptance matrix (loto-vr2 build order step 5/6) ---
//
// brokenLOTOBase points LOTO_BASE at a path that already exists as a
// regular file, so os.MkdirAll(dir, 0o700) inside openRuntime fails —
// simulating an unreachable store without touching internal/store.

func brokenLOTOBase(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGateCLI_ForeignClaimDeniesNotOnDiskTarget: kuv.10 class — a claim on
// internal/store denies a not-yet-on-disk internal/store/newfile.go. Proves
// resolveCLITarget never stats the target for the gate path.
func TestGateCLI_ForeignClaimDeniesNotOnDiskTarget(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	target := tcPrefixStore + "/newfile.go" // not on disk
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, target}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ blocked count=1") || !strings.Contains(out.String(), "kind=claim") {
		t.Errorf("expected claim-kind deny row: %q", out.String())
	}
	if !strings.Contains(out.String(), "options=wait|pick-other-work|message-holder") {
		t.Errorf("expected options line, no fix command, on a claim row: %q", out.String())
	}
}

// TestGateCLI_OwnClaimAllows: a claim held by the checking agent itself must
// not deny its own targets.
func TestGateCLI_OwnClaimAllows(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("claim failed")
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (own claim), got %d: %q", code, out.String())
	}
}

// TestGateCLI_ExpiredClaimAllows: a lapsed claim must not deny.
func TestGateCLI_ExpiredClaimAllows(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest, tcFlagTTL, "1ms"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}
	time.Sleep(20 * time.Millisecond)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (expired claim), got %d: %q", code, out.String())
	}
}

// TestGateCLI_ForeignSharedBeaconDeniesButPlainCheckAllows pins the
// deliberate gate-vs-plain-check divergence (plan point 1): the same
// on-disk state reads as a deny under --gate and a pass under plain check.
func TestGateCLI_ForeignSharedBeaconDeniesButPlainCheckAllows(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice shared lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var gateOut bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &gateOut, &bytes.Buffer{}); code != 1 {
		t.Fatalf("expected gate exit 1 for foreign shared beacon, got %d: %q", code, gateOut.String())
	}
	if !strings.Contains(gateOut.String(), "kind=lock") {
		t.Errorf("expected lock-kind deny row: %q", gateOut.String())
	}

	var plainOut bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcTargetA}, &plainOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("plain check must still pass shared-vs-shared, got %d: %q", code, plainOut.String())
	}
}

// TestGateCLI_OwnBeaconAllows: a shared beacon the checking agent holds
// itself must not deny.
func TestGateCLI_OwnBeaconAllows(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("shared lock failed")
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (own beacon), got %d: %q", code, out.String())
	}
}

// TestGateCLI_ForeignExclusiveLiveDenies: a durable, provably-live exclusive
// holder denies under the gate, same as plain check's hard-block case.
func TestGateCLI_ForeignExclusiveLiveDenies(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "kind=lock") {
		t.Errorf("expected lock-kind deny row: %q", out.String())
	}
	if !strings.Contains(out.String(), "loto unlock --force") {
		t.Errorf("expected unlock --force fix block on a lock row: %q", out.String())
	}
}

// TestGateCLI_StaleForeignLockAllows: a TTL-lapsed holder is reclaimable —
// it must not deny.
func TestGateCLI_StaleForeignLockAllows(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, "1ms", "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	time.Sleep(20 * time.Millisecond)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (stale holder reclaimable), got %d: %q", code, out.String())
	}
}

// TestGateCLI_NoIdentityFailsOpenWithoutOpeningStore: env cleared (no
// LOTO_AGENT_ID/LOTO_SUBAGENT_ID/CLAUDE_CODE_SESSION_ID) + a broken LOTO_BASE
// must still fail open exit 0 — proving the identity short-circuit runs
// BEFORE openRuntime, since a broken LOTO_BASE would otherwise surface as
// exit 3 (loto-vr2 hard rule: no store IO on the human-shell path).
func TestGateCLI_NoIdentityFailsOpenWithoutOpeningStore(t *testing.T) {
	withTempProject(t)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("LOTO_SUBAGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	t.Setenv("LOTO_BASE", brokenLOTOBase(t))

	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("expected exit 0 (unpinned fail-open), got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "identity=unpinned gate=fail-open") {
		t.Errorf("expected unpinned fail-open row: %q", out.String())
	}
	if strings.Contains(out.String(), "store=unreachable") {
		t.Errorf("must not have attempted to open the store: %q", out.String())
	}
}

// TestGateCLI_StoreUnreachableFailsOpen: a pinned identity with a broken
// LOTO_BASE must fail open exit 3 with the store=unreachable row.
func TestGateCLI_StoreUnreachableFailsOpen(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_BASE", brokenLOTOBase(t))

	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &bytes.Buffer{})
	if code != 3 {
		t.Fatalf("expected exit 3, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "⚠ store=unreachable gate=fail-open") {
		t.Errorf("expected store-unreachable fail-open row: %q", out.String())
	}
}

// TestGateCLI_SymmetricRootDeniedBySubagentClaim: root's session-pinned
// identity (no LOTO_SUBAGENT_ID) is denied by a sibling subagent's claim
// like any other peer — symmetric root policy (gate-design.md "Rules").
func TestGateCLI_SymmetricRootDeniedBySubagentClaim(t *testing.T) {
	withTempProject(t)
	root := pinAgent(t) // session-pinned root identity

	// A /team sibling: distinct owner via LOTO_SUBAGENT_ID, claims territory
	// covering root's target while nominally still under root's LOTO_AGENT_ID
	// (a subagent inherits the parent's LOTO_AGENT_ID; LOTO_SUBAGENT_ID takes
	// priority in identity.Ensure regardless — see resolveSubagent).
	t.Setenv("LOTO_SUBAGENT_ID", "sibling-1")
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("sibling claim failed")
	}
	os.Unsetenv("LOTO_SUBAGENT_ID")

	// Back to root: LOTO_AGENT_ID pinned to root's own UUID, no subagent id.
	t.Setenv("LOTO_AGENT_ID", root.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreStoreGo}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected root denied by sibling claim, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "kind=claim") {
		t.Errorf("expected claim-kind deny row: %q", out.String())
	}
}

// TestGateCLI_MultiPathDeniesOnAnyBlocked: two paths, only one denied —
// overall exit 1, exactly one deny row (the untouched sibling path).
func TestGateCLI_MultiPathDeniesOnAnyBlocked(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA, tcTargetB}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ blocked count=1") {
		t.Errorf("expected exactly one deny row, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "path="+tcTargetA) || strings.Contains(out.String(), "path="+tcTargetB) {
		t.Errorf("expected a deny row for %s only, got: %q", tcTargetA, out.String())
	}
}

// TestGateCLI_InvalidTargetReturnsExit2: a repo-escaping absolute path is
// rejected the same way plain check rejects it.
func TestGateCLI_InvalidTargetReturnsExit2(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, "/etc/hosts"}, &out, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ invalid") || !strings.Contains(out.String(), "/etc/hosts") {
		t.Errorf("expected invalid report citing /etc/hosts: %q", out.String())
	}
}
