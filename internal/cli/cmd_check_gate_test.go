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
	"loto/internal/render"
)

const tcFlagGate = "--gate"

// gateDecide unit tests (loto-vr2, gate-design.md component 4). Pure —
// domain.LockRecord/domain.ClaimRecord fixtures in, []render.GateDenyRow
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

// deadProbe / aliveProbe stand in for runtime.liveProbe in unit tests: the
// probe owns all environmental policy, so pinning its verdict is the only way
// to exercise the liveness leg of the claim predicate (loto-tzmv.9). They
// answer unconditionally because a claim carries no PID — a pid-gated stub
// would return UNKNOWN for every claim and silently skip the leg under test.
func deadProbe(domain.LockRecord) domain.Liveness  { return domain.LivenessDead }
func aliveProbe(domain.LockRecord) domain.Liveness { return domain.LivenessAlive }

func TestGateDecide_ForeignLiveClaimDenies(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/new.go"} // kuv.10 class: not on disk
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "foe-claim", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 1 {
		t.Fatalf("want 1 deny row, got %d: %+v", len(rows), rows)
	}
	if rows[0].Kind != render.GateKindClaim || rows[0].HolderUUID != gateFoeUUID || rows[0].Path != target.Canonical {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}

func TestGateDecide_OwnClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: gateMyUUID, Intent: gateIntentMine, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own claim must not deny, got %+v", rows)
	}
}

func TestGateDecide_ExpiredClaimAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/new.go"}
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "stale", ExpiresAt: now.Add(-time.Minute)},
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
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].Kind != render.GateKindLock || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("want 1 lock-kind deny row, got %+v", rows)
	}
}

// The path-scoped gate deliberately has NO same-session carve-out — the
// opposite of gateDecideAny (loto-xwod). Denying a sibling's write to a path
// another sibling is writing IS the bead: on 2026-08-14 two subagents of one
// session wrote the same files concurrently and one's work was destroyed.
func TestGateDecide_SiblingBeaconDeniesWrite(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, SessionUUID: gateMySession,
			Mode: domain.ModeShared, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("a sibling's beacon must deny this sibling's write, got %+v", rows)
	}
}

// Re-entrancy: an agent's own beacon, re-minted on every write to the same
// path, must never block that agent's next write.
func TestGateDecide_OwnBeaconAllowsWrite(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, SessionUUID: gateMySession,
			Mode: domain.ModeShared, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 0 {
		t.Fatalf("own beacon must not deny own write, got %+v", rows)
	}
}

func TestGateDecide_ForeignStaleLockAllows(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcTargetA}
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
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateMyUUID, Mode: domain.ModeExclusive, Intent: gateIntentMine, ExpiresAt: now.Add(time.Hour)},
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
	target := domain.Target{Canonical: tcTargetA}
	locks := []domain.LockRecord{
		{Target: target, OwnerUUID: gateFoeUUID, Mode: domain.ModeShared, PID: 0, Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour)},
	}
	ec := gateEC(now)
	if ec.Classify(locks[0]) != domain.LivenessUnknown {
		t.Fatalf("test fixture invariant broken: want LivenessUnknown, got %v", ec.Classify(locks[0]))
	}
	rows := gateDecide([]domain.Target{target}, locks, nil, gateMyUUID, ec)
	if len(rows) != 1 || rows[0].Kind != render.GateKindLock {
		t.Fatalf("PID-0 beacon within TTL must deny, got %+v", rows)
	}
}

// TestGateDecide_DedupeAndOrder: a claim AND a lock both cover the same
// path from the same foreign owner must not collapse into one row (distinct
// kinds), a duplicate target in the input must not double-count, and output
// is sorted path -> kind -> holder UUID regardless of input order.
func TestGateDecide_DedupeAndOrder(t *testing.T) {
	now := time.Now()
	tB := domain.Target{Canonical: tcTargetB}
	aPath := tcPrefixStore + "/a.go"
	tA := domain.Target{Canonical: aPath}
	locks := []domain.LockRecord{
		{Target: tB, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, Intent: "lockrow", ExpiresAt: now.Add(time.Hour)},
	}
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "claimrow", ExpiresAt: now.Add(time.Hour)},
	}
	// tB passed twice (duplicate CLI arg) + tA once, out of sorted order.
	rows := gateDecide([]domain.Target{tB, tA, tB}, locks, claims, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 deduped rows (one per distinct path), got %d: %+v", len(rows), rows)
	}
	// tcTargetB ("b.go") sorts before aPath ("internal/store/a.go") — 'b' < 'i'.
	if rows[0].Path != tcTargetB || rows[1].Path != aPath {
		t.Fatalf("want path-sorted rows, got %+v", rows)
	}
	if rows[0].Kind != render.GateKindLock || rows[1].Kind != render.GateKindClaim {
		t.Fatalf("unexpected kinds: %+v", rows)
	}
}

// TestGateDecide_SameOwnerTwoPrefixes_BlockerPathTieBreak: one owner holding
// claims at two ancestor prefixes of one target yields two rows identical in
// path/kind/holder — the blocker-path tie-break must order them ascending
// regardless of claim input order (review, #211).
func TestGateDecide_SameOwnerTwoPrefixes_BlockerPathTieBreak(t *testing.T) {
	now := time.Now()
	target := domain.Target{Canonical: tcPrefixStore + "/sub/new.go"}
	claims := []domain.ClaimRecord{ // deeper prefix first: reversed vs sorted order
		{PathPrefix: tcPrefixStore + "/sub", OwnerUUID: gateFoeUUID, Intent: "deep", ExpiresAt: now.Add(time.Hour)},
		{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "shallow", ExpiresAt: now.Add(time.Hour)},
	}
	rows := gateDecide([]domain.Target{target}, nil, claims, gateMyUUID, gateEC(now))
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (distinct blocker prefixes), got %d: %+v", len(rows), rows)
	}
	if rows[0].BlockerPath != tcPrefixStore || rows[1].BlockerPath != tcPrefixStore+"/sub" {
		t.Errorf("want blocker-path ascending tie-break, got %+v", rows)
	}
}

// gateDecideAny unit tests (ccp-vx4w): the repo-wide sibling of gateDecide,
// used by `loto guard`. Table-driven per ADR-008 — one lock/claim fixture
// set per row, no target list since the predicate is path-free.
func TestGateDecideAny(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		locks   []domain.LockRecord
		claims  []domain.ClaimRecord
		live    domain.HolderLiveProbe // nil = TTL is the sole authority
		session domain.SessionUUID     // "" = no CLAUDE_CODE_SESSION_ID, direct CLI use
		want    int
	}{
		{
			name: "foreign live lock denies",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			want: 1,
		},
		{
			name: "own lock allows",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Intent: gateIntentMine, ExpiresAt: now.Add(time.Hour)},
			},
			want: 0,
		},
		{
			name: "stale foreign lock allows",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, Intent: gateIntentFoe, ExpiresAt: now.Add(-time.Hour)},
			},
			want: 0,
		},
		{
			name: "foreign live claim denies",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "foe claim", ExpiresAt: now.Add(time.Hour)},
			},
			want: 1,
		},
		{
			name: "expired foreign claim allows",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "stale", ExpiresAt: now.Add(-time.Minute)},
			},
			want: 0,
		},
		{
			name: "no locks or claims allows",
			want: 0,
		},
		{
			// loto-tzmv.9: an unexpired claim whose owner is provably dead must
			// stop denying. Before the fix this row denied for the claim's full
			// 2h TTL, freezing every tree-move in the repo behind one crash.
			name: "unexpired claim from a dead owner allows",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "crashed", ExpiresAt: now.Add(time.Hour)},
			},
			live: deadProbe,
			want: 0,
		},
		{
			// The other half of the same predicate: liveness must not weaken a
			// live owner's claim.
			name: "unexpired claim from a live owner denies",
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "working", ExpiresAt: now.Add(time.Hour)},
			},
			live: aliveProbe,
			want: 1,
		},
		{
			// loto-xwod: a beacon minted for a SIBLING subagent of this same
			// Claude session says "an agent of mine is writing here". It must
			// deny that sibling's peer at write time (gateDecide) without
			// freezing the session's own `git checkout`.
			name: "sibling beacon allows this session's tree-move",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, SessionUUID: gateMySession,
					Mode: domain.ModeShared, PID: 0, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			session: gateMySession,
			want:    0,
		},
		{
			// The carve-out is beacons only. A sibling's real exclusive lock is a
			// declaration of uncommitted territory, and a checkout under it is the
			// 2026-08-14 incident itself.
			name: "sibling's exclusive lock still denies the tree-move",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, SessionUUID: gateMySession,
					Mode: domain.ModeExclusive, PID: 4242, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			session: gateMySession,
			want:    1,
		},
		{
			name: "another session's beacon denies the tree-move",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, SessionUUID: gateFoeSession,
					Mode: domain.ModeShared, PID: 0, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			session: gateMySession,
			want:    1,
		},
		{
			// An empty session id must match nothing rather than everything —
			// direct CLI use outside Claude Code has no session to be a sibling of.
			name: "beacon carve-out needs a known session",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, SessionUUID: "",
					Mode: domain.ModeShared, PID: 0, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			session: "",
			want:    1,
		},
		{
			name: "one foreign lock plus one foreign claim denies both",
			locks: []domain.LockRecord{
				{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
			},
			claims: []domain.ClaimRecord{
				{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: "foe claim", ExpiresAt: now.Add(time.Hour)},
			},
			want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ec := gateEC(now)
			ec.Live = c.live
			rows := gateDecideAny(c.locks, c.claims, gateMyUUID, c.session, ec)
			if len(rows) != c.want {
				t.Fatalf("gateDecideAny() = %d rows, want %d: %+v", len(rows), c.want, rows)
			}
		})
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
	if strings.Contains(out.String(), "loto unlock --force") {
		t.Errorf("claim row must not carry the lock-only fix command: %q", out.String())
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

// TestGateCLI_MalformedSubagentIDFailsOpen: a traversal-shaped
// LOTO_SUBAGENT_ID is ignored by identity.Ensure (resolveSubagent falls
// open), so with no other identity env Ensure would mint a throwaway UUID
// owning nothing — every foreign row would deny. The gate must treat that id
// as unpinned and fail open BEFORE store IO (review P1, #211).
func TestGateCLI_MalformedSubagentIDFailsOpen(t *testing.T) {
	withTempProject(t)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	t.Setenv("LOTO_SUBAGENT_ID", "../escape")
	t.Setenv("LOTO_BASE", brokenLOTOBase(t)) // proves no store IO happens

	var out, errb bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagGate, tcTargetA}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected exit 0 (malformed subagent id fails open), got %d: %q", code, errb.String())
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
