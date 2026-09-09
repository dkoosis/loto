package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"loto/internal/domain"
	"loto/internal/store"
)

// tcPhasePrepared is git's one refusable reference-transaction phase.
const tcPhasePrepared = "prepared"

// The measured transaction lines. Every one of these was captured from git
// 2.55.0 with a logging reference-transaction hook in a scratch repo — this
// table is the specification (§10a), not an illustration, so a git that
// changes any of these shapes fails here rather than in production.
const (
	tcOIDZero = "0000000000000000000000000000000000000000"
	tcOIDA    = "1ae0b17de3a046f4f2bee39c74cdb4aee10622d8"
	tcOIDB    = "f5c0aa1577c1f9cd46cb1999b36e5c5670c8e3cc"
	tcRefMain = "refs/heads/main"
)

func TestClassifyRefUpdate_MeasuredShapes(t *testing.T) {
	cases := []struct {
		name string
		u    refUpdate
		want string
	}{
		// ── refused ──
		{"checkout other", refUpdate{tcOIDZero, "ref:refs/heads/other", tcHEAD}, refShapeHeadSymref},
		{"checkout -b new (HEAD leg)", refUpdate{tcOIDZero, "ref:refs/heads/newb", tcHEAD}, refShapeHeadSymref},
		{"worktree add -b (HEAD leg)", refUpdate{tcOIDZero, "ref:refs/heads/zb", tcHEAD}, refShapeHeadSymref},
		{"stash push", refUpdate{tcOIDZero, tcOIDB, "refs/stash"}, refShapeStash},
		{"stash pop", refUpdate{tcOIDB, tcOIDZero, "refs/stash"}, refShapeStash},
		{"branch -D loose", refUpdate{tcOIDZero, tcOIDZero, "refs/heads/newb"}, refShapeBranchDelete},
		{"branch -D packed", refUpdate{tcOIDZero, tcOIDZero, "refs/heads/packedonly"}, refShapeBranchDelete},
		{"update-ref -d with old", refUpdate{tcOIDZero, tcOIDZero, "refs/heads/other"}, refShapeBranchDelete},

		// ── admitted ──
		{"commit moves the tip", refUpdate{tcOIDZero, tcOIDA, tcRefMain}, ""},
		{"reset --hard on current branch", refUpdate{tcOIDA, tcOIDA, tcRefMain}, ""},
		{"branch creation", refUpdate{tcOIDZero, tcOIDA, "refs/heads/zb"}, ""},
		{"ORIG_HEAD", refUpdate{tcOIDZero, tcOIDA, "ORIG_HEAD"}, ""},
		{"AUTO_MERGE", refUpdate{tcOIDZero, tcOIDZero, "AUTO_MERGE"}, ""},
		{"fetch --prune deletes a remote", refUpdate{tcOIDZero, tcOIDZero, "refs/remotes/origin/gone"}, ""},
		{"fetch writes a remote", refUpdate{tcOIDZero, tcOIDA, "refs/remotes/origin/main"}, ""},
		// pack-refs removes each LOOSE file after packed-refs already holds
		// the ref, and reports the real old oid. Refusing this breaks
		// `git pack-refs` and `git gc` (measured: exit 128).
		{"pack-refs loose cleanup", refUpdate{tcOIDA, tcOIDZero, tcRefMain}, ""},
		{"pack-refs writes packed-refs", refUpdate{tcOIDZero, tcOIDA, tcRefMain}, ""},
		{"tag deletion", refUpdate{tcOIDZero, tcOIDZero, "refs/tags/v1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRefUpdate(tc.u); got != tc.want {
				t.Errorf("classifyRefUpdate(%+v) = %q, want %q", tc.u, got, tc.want)
			}
		})
	}
}

func TestParseRefTransaction(t *testing.T) {
	in := strings.Join([]string{
		tcOIDZero + " ref:refs/heads/other HEAD",
		tcOIDZero + " " + tcOIDA + " refs/heads/main",
		"garbage-without-fields",
		tcOIDZero + " lonely",
		"",
	}, "\n")
	got := parseRefTransaction(strings.NewReader(in))
	if len(got) != 2 {
		t.Fatalf("want 2 parsed updates from a stream with 2 well-formed lines, got %d: %+v", len(got), got)
	}
	// The symref value keeps its colon and its whole ref path — cutting on the
	// first two spaces is what preserves it.
	if got[0].New != "ref:refs/heads/other" || got[0].Ref != tcHEAD {
		t.Errorf("symref line mis-parsed: %+v", got[0])
	}
	if got[1].Ref != tcRefMain || got[1].New != tcOIDA {
		t.Errorf("oid line mis-parsed: %+v", got[1])
	}
}

func TestRefZeroOID(t *testing.T) {
	for _, s := range []string{tcOIDZero, strings.Repeat("0", 64)} {
		if !refZeroOID(s) {
			t.Errorf("refZeroOID(%q) = false, want true (sha1 and sha256 null oids)", s)
		}
	}
	for _, s := range []string{"", tcOIDA, "ref:refs/heads/x"} {
		if refZeroOID(s) {
			t.Errorf("refZeroOID(%q) = true, want false", s)
		}
	}
}

// runRefHook drives the runner with a transaction on stdin and returns
// (exit, stdout, stderr).
func runRefHook(t *testing.T, phase, txn string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runHookRef(context.Background(), phase, strings.NewReader(txn), &out, &errb)
	return code, out.String(), errb.String()
}

// txnCheckoutOther is the transaction `git checkout other` issues.
const txnCheckoutOther = tcOIDZero + " ref:refs/heads/other HEAD\n"

func TestRunHookRef_NonPreparedPhaseNeverRefuses(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	plantLiveSessionRecord(t, "sess-a", "owner-a")
	plantLiveSessionRecord(t, "sess-b", "owner-b")

	for _, phase := range []string{"preparing", "committed", "aborted"} {
		code, _, stderr := runRefHook(t, phase, txnCheckoutOther)
		if code != 0 {
			t.Errorf("phase %s: exit %d, want 0 — only `prepared` can abort a transaction; %s", phase, code, stderr)
		}
	}
}

func TestRunHookRef_AdmittedShapePassesWithoutTouchingTheStore(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	plantLiveSessionRecord(t, "sess-a", "owner-a")
	plantLiveSessionRecord(t, "sess-b", "owner-b")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" "+tcOIDA+" refs/heads/main\n")
	if code != 0 {
		t.Fatalf("a branch tip moving must pass: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "✓ ref-allowed") {
		t.Errorf("pass needs an explicit status header (design.md): %q", stdout)
	}
}

func TestRunHookRef_NoSessionIDPasses(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	plantLiveSessionRecord(t, "sess-a", "owner-a")
	plantLiveSessionRecord(t, "sess-b", "owner-b")
	// A bare shell: no session id in the inherited environment, so the caller
	// is not in S and claims no protection from I1.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOTO_SESSION_ID", "")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("a hook with no session id must pass: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "session=absent") {
		t.Errorf("the pass must say why it passed: %q", stdout)
	}
}

func TestRunHookRef_OneLiveSessionPasses(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	plantLiveSessionRecord(t, "sess-a", "owner-a")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("|S| = 1 has no peer to protect: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "live=1") {
		t.Errorf("the pass must carry |S|: %q", stdout)
	}
}

func TestRunHookRef_TwoLiveSessionsRefusesAndCounts(t *testing.T) {
	withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionRecord(t, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-peer", "peer-owner-uuid-0001")

	code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 1 {
		t.Fatalf("a branch switch under two live sessions must be refused: exit %d, %s", code, stderr)
	}
	for _, want := range []string{
		"✗ ref-refused count=1 live=2",
		"shape=" + refShapeHeadSymref,
		"peer-owner-uuid-0001",
		// The override, verbatim — a refusal a reader cannot act on is a
		// refusal they will disable.
		`loto claim . -t "<reason>"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal stderr missing %q; got:\n%s", want, stderr)
		}
	}

	ev := latestRefEvent(t, store.EventRefRefused)
	if ev.Reason != refShapeHeadSymref {
		t.Errorf("event reason = %q, want the refused shape %q", ev.Reason, refShapeHeadSymref)
	}
	if ev.Target.Canonical != tcHEAD {
		t.Errorf("event target = %q, want the ref name", ev.Target.Canonical)
	}
	if ev.ActorUUID != a.UUID {
		t.Errorf("event actor = %q, want the refused owner %q", ev.ActorUUID, a.UUID)
	}
	// §10b row 1: (old, new, ref, |S|) — the four fields the counter needs.
	for _, want := range []string{`"old":"` + tcOIDZero, `"new":"ref:refs/heads/other"`, `"ref":"` + tcHEAD + `"`, `"live":2`} {
		if !strings.Contains(ev.Detail, want) {
			t.Errorf("ref_refused detail missing %s; got %s", want, ev.Detail)
		}
	}
}

func TestRunHookRef_StashAndBranchDeleteRefused(t *testing.T) {
	withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionRecord(t, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-peer", "peer-owner-uuid-0001")

	for _, tc := range []struct{ name, txn, shape string }{
		{"stash", tcOIDZero + " " + tcOIDB + " refs/stash\n", refShapeStash},
		{"branch delete", tcOIDZero + " " + tcOIDZero + " refs/heads/x\n", refShapeBranchDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runRefHook(t, tcPhasePrepared, tc.txn)
			if code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, stderr)
			}
			if !strings.Contains(stderr, "shape="+tc.shape) {
				t.Errorf("refusal must name the shape %q; got:\n%s", tc.shape, stderr)
			}
		})
	}
}

func TestRunHookRef_CheckoutWideClaimOverrides(t *testing.T) {
	withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionRecord(t, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-peer", "peer-owner-uuid-0001")

	if _, _, code := executeCommand("claim", ".", tcFlagIntent, "takeover: rebasing every lane"); code != 0 {
		t.Fatalf("claim . exit %d", code)
	}
	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("the checkout-wide claim is the override: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "override=claim") {
		t.Errorf("the pass must name the override it honored: %q", stdout)
	}
}

// TestRunHookRef_PeerClaimDoesNotOverride: the override is the CALLER's claim.
// A peer holding the checkout does not license this session to move the tree.
func TestRunHookRef_PeerClaimDoesNotOverride(t *testing.T) {
	repo := withTempProject(t)
	peer := pinAgent(t)
	if _, _, code := executeCommand("claim", ".", tcFlagIntent, "peer takeover"); code != 0 {
		t.Fatalf("peer claim . exit %d", code)
	}
	self := pinAgent(t)
	_ = repo
	plantLiveSessionRecord(t, "sess-self", self.UUID)
	plantLiveSessionRecord(t, "sess-peer", peer.UUID)

	code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 1 {
		t.Fatalf("a peer's checkout-wide claim must not override this caller: exit %d, %s", code, stderr)
	}
}

// TestClaimRoot_AfterRefusalWritesOverrideCounter is §10b row 1's second half:
// the ratio needs the override, and the override is the claim, not the hook.
func TestClaimRoot_AfterRefusalWritesOverrideCounter(t *testing.T) {
	withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionRecord(t, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-peer", "peer-owner-uuid-0001")

	if code, _, _ := runRefHook(t, tcPhasePrepared, txnCheckoutOther); code != 1 {
		t.Fatalf("fixture: the refusal must happen first, got exit %d", code)
	}
	if _, _, code := executeCommand("claim", ".", tcFlagIntent, "the guard was wrong"); code != 0 {
		t.Fatalf("claim . exit %d", code)
	}
	ev := latestRefEvent(t, store.EventRefRefusedOverridden)
	if ev.ActorUUID != a.UUID {
		t.Errorf("override event actor = %q, want %q", ev.ActorUUID, a.UUID)
	}
	if ev.Reason != refShapeHeadSymref {
		t.Errorf("the override must carry the shape it overrode: %q", ev.Reason)
	}
}

// TestClaimSubdir_WritesNoOverrideCounter: only the CHECKOUT-WIDE claim is an
// override. A claim on one package after an unrelated refusal is ordinary
// territory work and must not inflate the ratio.
func TestClaimSubdir_WritesNoOverrideCounter(t *testing.T) {
	withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionRecord(t, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-peer", "peer-owner-uuid-0001")

	if code, _, _ := runRefHook(t, tcPhasePrepared, txnCheckoutOther); code != 1 {
		t.Fatal("fixture: expected a refusal")
	}
	if _, _, code := executeCommand("claim", "internal/store", tcFlagIntent, "store refactor"); code != 0 {
		t.Fatalf("claim exit %d", code)
	}
	evs := readAllEvents(t)
	for i := range evs {
		if evs[i].Kind == store.EventRefRefusedOverridden {
			t.Fatal("a sub-prefix claim is not an override of I1")
		}
	}
}

// latestRefEvent returns the newest event of kind, failing if there is none.
func latestRefEvent(t *testing.T, kind string) domain.Event {
	t.Helper()
	var found *domain.Event
	evs := readAllEvents(t)
	for i := range evs {
		if evs[i].Kind == kind {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatalf("no %s event was written", kind)
	}
	return *found
}

func readAllEvents(t *testing.T) []domain.Event {
	t.Helper()
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer rt.Close()
	evs, err := rt.Store.ListEvents(rt.Ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return evs
}
