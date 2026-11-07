package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
)

const (
	tcFlagGate   = "--gate"
	tcFlagStaged = "--staged"
	tcFlagBranch = "--branch"
)

// gateDecide unit tests (loto-vr2, gate-design.md component 4). Pure —
// domain.LockRecord fixtures in, []render.GateDenyRow
// out — no store, no CLI, no clock read beyond the EvalContext/now passed
// in. These
// pin the deliberate divergences from plain check's computeCheckConflicts:
// any-mode foreign-lock deny, claims consulted at all, and !IsStale (not
// Classify==Alive) as the liveness threshold.

const (
	gateMyUUID       = "11111111-1111-1111-1111-111111111111"
	gateFoeUUID      = "22222222-2222-2222-2222-222222222222"
	gateIntentMine   = "mine"
	gateIntentFoe    = "foe edit"
	gateIntentBeacon = "beacon"
	// Sibling subagents of one Claude session hold DISTINCT owner uuids and
	// SHARE a session uuid — that pair is what the beacon carve-out reads
	// (loto-xwod).
	gateMySession  domain.SessionUUID = "33333333-3333-3333-3333-333333333333"
	gateFoeSession domain.SessionUUID = "44444444-4444-4444-4444-444444444444"
)

func gateEC(now time.Time) domain.EvalContext {
	return domain.EvalContext{Now: now, Live: nil}
}

func TestGateDecide_ForeignDirectoryHoldDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/new.go"} // kuv.10 class: not on disk
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcPrefixStore}, OwnerUUID: gateFoeUUID, Intent: "foe-claim", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 1 {
		t.Fatalf("want 1 deny row, got %d: %+v", len(rows), rows)
	}
	if rows[0].HolderUUID != gateFoeUUID || rows[0].Path != target.Canonical || rows[0].BlockerPath != tcPrefixStore {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}

func TestGateDecide_ForeignSharedBeaconDenies(t *testing.T) {
	// Any-mode divergence from plain check: a foreign SHARED lock still denies
	// under the gate (plan point 1) — plain check's shared-vs-shared probe
	// would pass this.
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].Kind != render.GateKindLock || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("want 1 lock-kind deny row, got %+v", rows)
	}
}

// The path-scoped gate deliberately has NO same-session carve-out — the
// opposite of gateDecideAny (loto-xwod). Denying a sibling's write to a path
// another sibling is writing IS the bead: on 2026-08-14 two subagents of one
// session wrote the same files concurrently and one's work was destroyed.
func TestGateDecide_SiblingHoldDeniesWrite(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, SessionUUID: gateMySession, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("a sibling's beacon must deny this sibling's write, got %+v", rows)
	}
}

// Re-entrancy: an agent's own beacon, re-minted on every write to the same
// path, must never block that agent's next write.
func TestGateDecide_OwnHoldAllowsWrite(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, SessionUUID: gateMySession, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own beacon must not deny own write, got %+v", rows)
	}
}

func TestGateDecide_ForeignStaleLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Intent: "old", ExpiresAt: now.Add(-time.Minute)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("stale (TTL-lapsed) lock must not deny, got %+v", rows)
	}
}

func TestGateDecide_OwnLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, Intent: gateIntentMine, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own lock must not deny, got %+v", rows)
	}
}

// TestGateDecide_PID0ForeignHoldWithinTTLDenies pins the liveness-threshold
// divergence (plan point 3): the gate uses !IsStale, not Classify==Alive, so
// a hook-minted beacon with no durable PID (the sentinel PID<=0 — no
// LOTO_PID) still denies while its TTL hasn't lapsed. Classify would report
// this holder LivenessUnknown; plain check's Classify==Alive gate would only
// warn, never hard-block, on this row.
func TestGateDecide_PID0ForeignHoldWithinTTLDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	ec := gateEC(now)
	if ec.Classify(locks[0]) != domain.LivenessUnknown {
		t.Fatalf("test fixture invariant broken: want LivenessUnknown, got %v", ec.Classify(locks[0]))
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, ec)
	if len(rows) != 1 || rows[0].Kind != render.GateKindLock {
		t.Fatalf("PID-0 beacon within TTL must deny, got %+v", rows)
	}
}

// TestGateDecide_DedupeAndOrder: a duplicate target in the input must not
// double-count, and output is sorted path -> kind -> holder UUID regardless
// of input order.
func TestGateDecide_DedupeAndOrder(t *testing.T) {
	now := time.Now()
	tB := domain.Target{Canonical: tcTargetB}
	aPath := tcPrefixStore + "/a.go"
	tA := domain.Target{Canonical: aPath}
	locks := []domain.LockRecord{
		{Target: tB, OwnerUUID: gateFoeUUID, Intent: "lockrow", ExpiresAt: now.Add(time.Hour)},
		{Target: domain.Target{Canonical: tcPrefixStore}, OwnerUUID: gateFoeUUID, Intent: "dirrow", ExpiresAt: now.Add(time.Hour)},
	}
	// tB passed twice (duplicate CLI arg) + tA once, out of sorted order.
	rows := gateDecide([]domain.Target{tB, tA, tB}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 deduped rows (one per distinct path), got %d: %+v", len(rows), rows)
	}
	// tcTargetB ("b.go") sorts before aPath ("internal/store/a.go") — 'b' < 'i'.
	if rows[0].Path != tcTargetB || rows[1].Path != aPath {
		t.Fatalf("want path-sorted rows, got %+v", rows)
	}
}

// TestGateDecide_SameOwnerTwoPrefixes_BlockerPathTieBreak: one owner holding
// two directories above one target yields two rows identical in
// path/kind/holder — the blocker-path tie-break must order them ascending
// regardless of input order (review, #211).
func TestGateDecide_SameOwnerTwoPrefixes_BlockerPathTieBreak(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/sub/new.go"}
	locks := []domain.LockRecord{ // deeper prefix first: reversed vs sorted order
		{Target: domain.Target{Canonical: tcPrefixStore + "/sub"}, OwnerUUID: gateFoeUUID, Intent: "deep", ExpiresAt: now.Add(time.Hour)},
		{Target: domain.Target{Canonical: tcPrefixStore}, OwnerUUID: gateFoeUUID, Intent: "shallow", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (distinct blocker prefixes), got %d: %+v", len(rows), rows)
	}
	if rows[0].BlockerPath != tcPrefixStore || rows[1].BlockerPath != tcPrefixStore+"/sub" {
		t.Errorf("want blocker-path ascending tie-break, got %+v", rows)
	}
}

// One owner holding a directory lock and a file lock that both cover a target
// yields a row for each lock (loto-f7b5), ordered by blocker path.
func TestGateDecide_SameOwnerTwoLocks_EachBlockerReported(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/new.go"}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Intent: "file", ExpiresAt: now.Add(time.Hour)},
		{Target: domain.Target{Canonical: tcPrefixStore}, OwnerUUID: gateFoeUUID, Intent: "dir", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per covering lock), got %d: %+v", len(rows), rows)
	}
	if rows[0].BlockerPath != tcPrefixStore || rows[1].BlockerPath != target.Canonical {
		t.Errorf("want blocker-path ascending, got %+v", rows)
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
	if !strings.Contains(out.String(), "✗ blocked count=1") || !strings.Contains(out.String(), "unlock --force -t \"unblock\" "+tcPrefixStore) {
		t.Errorf("expected a deny row naming the directory hold: %q", out.String())
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
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest, tcFlagTTL, tcTTL1ms}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
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

// TestGateCLI_OwnBeaconAllows: a lock the checking agent holds itself must
// not deny.
func TestGateCLI_OwnBeaconAllows(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRead}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
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
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, tcTTL1ms, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
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

	var out, errb bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected exit 0 (unpinned fail-open), got %d: %q", code, errb.String())
	}
	assertFailOpenStream(t, &out, &errb, "identity=unpinned gate=fail-open")
	if strings.Contains(errb.String(), "store=unreachable") {
		t.Errorf("must not have attempted to open the store: %q", errb.String())
	}
}

// assertFailOpenStream pins loto-tzmv.8: a fail-open notice must reach STDERR
// and must never appear on stdout. A PreToolUse hook exiting 0 with stdout
// output does not surface that output to the model, so a stdout-only notice
// means an ungated session believes it is gated. The stream is part of the
// contract, not a formatting detail — assert it on every fail-open branch so
// it cannot silently regress.
func assertFailOpenStream(t *testing.T, stdout, stderr *bytes.Buffer, want string) {
	t.Helper()
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("expected %q on stderr, got stderr=%q stdout=%q", want, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), "fail-open") {
		t.Errorf("fail-open notice leaked to stdout (hooks exiting 0 hide stdout from the model): %q", stdout.String())
	}
}

// TestGateCLI_SetButEmptyAgentIDFailsOpen: LOTO_AGENT_ID set to "" is the
// caller opting into an ephemeral identity — identity.Ensure mints a
// throwaway UUID that owns nothing. The shared agentIdentityPinned predicate
// (runtime.go, loto-s3l) reads set-but-empty as UNPINNED — a throwaway owner
// is foreign to every live row, so treating it as pinned would fail CLOSED,
// the exact inversion of the gate's contract. Pin: live foreign claim
// covering the target + LOTO_AGENT_ID="" → exit 0 + the ⚠ identity=unpinned
// row (adherence review P2).
func TestGateCLI_SetButEmptyAgentIDFailsOpen(t *testing.T) {
	withTempProject(t)
	alice, _ := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}

	t.Setenv("LOTO_AGENT_ID", "") // set-but-empty: ephemeral identity
	os.Unsetenv("LOTO_SUBAGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	var out, errb bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcStoreStoreGo}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected exit 0 (ephemeral identity fails open), got %d: %q", code, errb.String())
	}
	assertFailOpenStream(t, &out, &errb, "identity=unpinned gate=fail-open")
}

// TestGateCLI_StoreUnreachableFailsOpen: a pinned identity with a broken
// LOTO_BASE must fail open exit 3 with the store=unreachable row.
func TestGateCLI_StoreUnreachableFailsOpen(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_BASE", brokenLOTOBase(t))

	var out, errb bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &errb)
	if code != 3 {
		t.Fatalf("expected exit 3, got %d: %q", code, errb.String())
	}
	assertFailOpenStream(t, &out, &errb, "⚠ store=unreachable gate=fail-open")
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

// TestGateCLI_InvalidTargetReturnsExit2: invalid targets are rejected the
// same way plain check rejects them, and the invalid block is sorted by
// path regardless of argv order — "z*.go" (glob) passed BEFORE "/etc/hosts"
// (repo-escape) must render after it ('/' < 'z'), matching the
// computeCheckConflicts sort precedent (adherence review P3).
func TestGateCLI_InvalidTargetReturnsExit2(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, "z*.go", "/etc/hosts"}, &out, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d: %q", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✗ invalid count=2") {
		t.Errorf("expected invalid count=2 header: %q", s)
	}
	hostsIdx := strings.Index(s, "/etc/hosts")
	globIdx := strings.Index(s, "z*.go")
	if hostsIdx == -1 || globIdx == -1 {
		t.Fatalf("expected both invalid rows: %q", s)
	}
	if hostsIdx > globIdx {
		t.Errorf("invalid rows must sort by path (/etc/hosts before z*.go): %q", s)
	}
}

// TestGateCLI_StagedGateIsRetired (loto-ojq3): a commit is not a write
// decision, so the staged-lock gate's two legs exit 0 even with a peer's live
// claim over the staged path. The path gate still denies the same path.
func TestGateCLI_StagedGateIsRetired(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdClaim, tcPrefixStore, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice claim failed")
	}
	staged := tcPrefixStore + "/staged.go"
	if err := os.MkdirAll(filepath.Join(repo, tcPrefixStore), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, staged), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "add", staged).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	for _, args := range [][]string{
		{tcCmdCheck, tcFlagGate, tcFlagStaged},
		{tcCmdCheck, "--held", tcFlagStaged},
	} {
		var out, errOut bytes.Buffer
		code := Run(args, &out, &errOut)
		if code != 0 || out.String() != "✓ staged-gate=retired: a commit carries what the write gate admitted\n" || errOut.Len() != 0 {
			t.Errorf("%v: exit %d stdout %q stderr %q, want 0, the retired line, no stderr", args, code, out.String(), errOut.String())
		}
	}
	if code := Run([]string{tcCmdCheck, tcFlagGate, staged}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("the path gate must still deny a peer's claim, got exit %d", code)
	}
}
