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
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

// tcPhasePrepared is git's one refusable reference-transaction phase.
const tcPhasePrepared = "prepared"

// The measured transaction lines. Every one of these was captured from git
// 2.55.0 with a logging reference-transaction hook in a scratch repo — this
// table is the specification (§10a), not an illustration, so a git that
// changes any of these shapes fails here rather than in production.
const (
	tcOIDZero   = "0000000000000000000000000000000000000000"
	tcOIDA      = "1ae0b17de3a046f4f2bee39c74cdb4aee10622d8"
	tcOIDB      = "f5c0aa1577c1f9cd46cb1999b36e5c5670c8e3cc"
	tcRefMain   = "refs/heads/main"
	tcPeerOwner = "peer-owner-uuid-0001"
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
	repo := withTempProject(t)
	pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-a", "owner-a")
	plantLiveSessionHere(t, repo, "sess-b", "owner-b")

	for _, phase := range []string{"preparing", "committed", "aborted"} {
		code, _, stderr := runRefHook(t, phase, txnCheckoutOther)
		if code != 0 {
			t.Errorf("phase %s: exit %d, want 0 — only `prepared` can abort a transaction; %s", phase, code, stderr)
		}
	}
}

func TestRunHookRef_AdmittedShapePassesWithoutTouchingTheStore(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-a", "owner-a")
	plantLiveSessionHere(t, repo, "sess-b", "owner-b")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" "+tcOIDA+" refs/heads/main\n")
	if code != 0 {
		t.Fatalf("a branch tip moving must pass: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "✓ ref-allowed") {
		t.Errorf("pass needs an explicit status header (design.md): %q", stdout)
	}
}

func TestRunHookRef_NoSessionIDPasses(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-a", "owner-a")
	plantLiveSessionHere(t, repo, "sess-b", "owner-b")
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
	repo := withTempProject(t)
	pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-a", "owner-a")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("|S| = 1 has no peer to protect: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "live=1") {
		t.Errorf("the pass must carry |S|: %q", stdout)
	}
}

func TestRunHookRef_TwoLiveSessionsRefusesAndCounts(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

	code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 1 {
		t.Fatalf("a branch switch under two live sessions must be refused: exit %d, %s", code, stderr)
	}
	for _, want := range []string{
		"✗ ref-refused count=1 live=2",
		"shape=" + refShapeHeadSymref,
		tcPeerOwner,
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
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

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
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

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
	plantLiveSessionHere(t, repo, "sess-self", self.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", peer.UUID)

	code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 1 {
		t.Fatalf("a peer's checkout-wide claim must not override this caller: exit %d, %s", code, stderr)
	}
}

// TestClaimRoot_AfterRefusalWritesOverrideCounter is §10b row 1's second half:
// the ratio needs the override, and the override is the claim, not the hook.
func TestClaimRoot_AfterRefusalWritesOverrideCounter(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

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
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

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

// plantLiveSessionHere is plantLiveSessionRecord with the checkout named —
// the only records that count toward |S| after loto-ea8y.4's review, since a
// session live in some other repo protects nothing here.
func plantLiveSessionHere(t *testing.T, repoTop, sid, ownerUUID string) {
	t.Helper()
	// The record must carry the toplevel EXACTLY as git reports it — that is
	// the string both `loto whoami` and the hook read, and on darwin it is the
	// /private-resolved form of a t.TempDir() path.
	if top, err := gitToplevel(repoTop); err == nil {
		repoTop = top
	}
	dir := filepath.Join(os.Getenv("LOTO_BASE"), "session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"session_id":%q,"uuid":%q,"pid":%d,"repo":%q,"recorded_at":%q}`,
		sid, ownerUUID, os.Getpid(), repoTop, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunHookRef_PeersInAnotherCheckoutDoNotCount is the review's must-fix:
// |S| was machine-wide, so one live session in an unrelated repo made THIS
// checkout refuse a branch switch with no peer here at all.
func TestRunHookRef_PeersInAnotherCheckoutDoNotCount(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	// Two live sessions, both in a different checkout.
	elsewhere := t.TempDir()
	plantLiveSessionHere(t, elsewhere, "sess-far-1", "far-owner-0001")
	plantLiveSessionHere(t, elsewhere, "sess-far-2", "far-owner-0002")

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("a session live in another repo is not a peer here: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "live=1") {
		t.Errorf("|S| must count only this checkout's sessions: %q", stdout)
	}
}

// TestRunHookRef_RepolessRecordDoesNotCount: a record written before the repo
// field existed cannot be shown to be here, and I1's failure direction is to
// pass rather than to refuse on a guess.
func TestRunHookRef_RepolessRecordDoesNotCount(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionRecord(t, "sess-legacy", "legacy-owner-0001")

	if code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther); code != 0 {
		t.Fatalf("a repo-less record must not make up |S|: exit %d, %s", code, stderr)
	}
}

// TestRunHookRef_RebaseReattachAllowed is the review's second must-fix.
// `git rebase` ends by re-attaching HEAD after the branch tip has already
// moved; refusing that leaves HEAD detached on a rebased branch and refuses
// the recovery checkout too. Staged here as the state git leaves at that
// moment: HEAD detached at exactly the branch's tip.
func TestRunHookRef_RebaseReattachAllowed(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

	branch := gitCurrentBranch(t, repo)
	gitRun(t, repo, "commit", "--allow-empty", "-m", "one")
	gitRun(t, repo, "checkout", "--detach")

	txn := tcOIDZero + " ref:" + refHeadsPrefix + branch + " " + tcHEAD + "\n"
	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txn)
	if code != 0 {
		t.Fatalf("re-attaching a detached HEAD where it already sits must pass: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "head=reattach") {
		t.Errorf("the pass must name the carve-out it took: %q", stdout)
	}
}

// TestRunHookRef_DetachedHeadElsewhereStillRefused keeps the carve-out narrow:
// detached at a DIFFERENT commit, pointing HEAD at a branch is a real tree
// move and stays refused.
func TestRunHookRef_DetachedHeadElsewhereStillRefused(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

	branch := gitCurrentBranch(t, repo)
	gitRun(t, repo, "commit", "--allow-empty", "-m", "one")
	gitRun(t, repo, "checkout", "--detach")
	gitRun(t, repo, "commit", "--allow-empty", "-m", "detached work")

	txn := tcOIDZero + " ref:" + refHeadsPrefix + branch + " " + tcHEAD + "\n"
	if code, _, stderr := runRefHook(t, tcPhasePrepared, txn); code != 1 {
		t.Fatalf("a detached HEAD at another commit is a real move: exit %d, %s", code, stderr)
	}
}

// TestClaimRoot_SecondClaimDoesNotDoubleCount: one refusal, two claims, one
// override row. Counting the refusal twice is the direction that makes
// overrides look like refusals and condemns `allow` on a miscount.
func TestClaimRoot_SecondClaimDoesNotDoubleCount(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)

	if code, _, _ := runRefHook(t, tcPhasePrepared, txnCheckoutOther); code != 1 {
		t.Fatal("fixture: expected a refusal")
	}
	for i := range 2 {
		if _, _, code := executeCommand("claim", ".", tcFlagIntent, "override"); code != 0 {
			t.Fatalf("claim %d exit %d", i, code)
		}
	}
	n := 0
	evs := readAllEvents(t)
	for i := range evs {
		if evs[i].Kind == store.EventRefRefusedOverridden {
			n++
		}
	}
	if n != 1 {
		t.Errorf("one refusal must produce one override row, got %d", n)
	}
}

func TestRefRefusalInCheckout(t *testing.T) {
	here := "/repos/here"
	for _, tc := range []struct {
		name, detail string
		want         bool
	}{
		{"same checkout", `{"repo":"/repos/here"}`, true},
		{"other checkout", `{"repo":"/repos/there"}`, false},
		{"pre-field row", `{"ref":"HEAD"}`, false},
		{"unparseable", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refRefusalInCheckout(tc.detail, here); got != tc.want {
				t.Errorf("refRefusalInCheckout(%q) = %v, want %v", tc.detail, got, tc.want)
			}
		})
	}
}

// gitToplevel resolves a repo path the way the CLI does, so a planted session
// record and the hook's own lookup name the same checkout.
func gitToplevel(repo string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output()
	return strings.TrimSpace(string(out)), err
}

// gitCurrentBranch / gitRun are the two git helpers these cases need.
func gitCurrentBranch(t *testing.T, repo string) string {
	t.Helper()
	// symbolic-ref, not rev-parse --abbrev-ref: this is called on a repo that
	// may have no commit yet, where an unborn branch has no oid to parse.
	out, err := exec.Command("git", "-C", repo, "symbolic-ref", "--short", tcHEAD).Output()
	if err != nil {
		t.Fatalf("git symbolic-ref --short HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// plantSessionForPID plants a live-shaped session record owned by a NAMED
// process, start-time included. plantLiveSessionHere always stamps the test
// binary's own pid and no start-time, which cannot express loto-2jgn's two
// cases: two ids sharing ONE process, and an id whose process is gone.
func plantSessionForPID(t *testing.T, repoTop, sid, ownerUUID string, pid int) {
	t.Helper()
	if top, err := gitToplevel(repoTop); err == nil {
		repoTop = top
	}
	dir := filepath.Join(os.Getenv("LOTO_BASE"), "session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	procStart, _ := identity.ProcStart(pid)
	body := fmt.Sprintf(`{"session_id":%q,"uuid":%q,"pid":%d,"proc_start":%d,"repo":%q,"recorded_at":%q}`,
		sid, ownerUUID, pid, procStart, repoTop, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reapedPID starts and reaps a trivial process, then hands back its pid — a
// pid that provably no longer runs.
func reapedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}

// TestRunHookRef_RotatedSelfIDIsNotAPeer is loto-2jgn end to end: after
// /clear this process owns two session ids, both recorded, both live. |S|
// read 2 and the guard refused every ref update in the checkout — naming the
// caller's own previous id as the peer it was protecting.
func TestRunHookRef_RotatedSelfIDIsNotAPeer(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	plantSessionForPID(t, repo, a.UUID, a.UUID, os.Getpid())
	plantSessionForPID(t, repo, "sess-precleared", "precleared-owner-01", os.Getpid())

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther)
	if code != 0 {
		t.Fatalf("this process's own pre-/clear id is not a peer: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "live=1") {
		t.Errorf("|S| must count one process once: %q", stdout)
	}
}

// TestRunHookRef_StaleSessionDoesNotBlockBranchDelete is loto-2jgn's second
// acceptance case: a prior session id whose process is gone leaves |S| = 1,
// so the branch delete git refused under the guard now exits 0.
func TestRunHookRef_StaleSessionDoesNotBlockBranchDelete(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	plantSessionForPID(t, repo, a.UUID, a.UUID, os.Getpid())
	plantSessionForPID(t, repo, "sess-stale", "stale-owner-0001", reapedPID(t))

	txn := tcOIDZero + " " + tcOIDZero + " refs/heads/merged\n"
	code, stdout, stderr := runRefHook(t, tcPhasePrepared, txn)
	if code != 0 {
		t.Fatalf("a dead session blocks nothing: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "live=1") {
		t.Errorf("|S| must exclude a session whose process is gone: %q", stdout)
	}
}

// ── loto-w0sx: the HEAD of a worktree being created ───────────────────────
//
// Measured on git 2.55.0. `git worktree add <path> -b <branch>` run from the
// shared checkout emits the new worktree's HEAD as
// `0000… ref:refs/heads/<branch> HEAD` with GIT_DIR UNSET and cwd still the
// shared checkout — byte-identical, row and environment alike, to the row
// `git checkout -b <branch>` emits for the shared checkout's own HEAD. The
// repo's state at the `prepared` phase is what separates them: git has
// already locked the exact HEAD it is about to write.

// plantUnbornWorktree stages the administrative directory `git worktree add`
// leaves in the common dir while it is mid-flight — HEAD.lock taken, a
// `locked` file reading "initializing", and no HEAD file yet. wtPath is where
// the worktree itself will land; it deliberately need not exist, because at
// this moment in git's own sequence it holds no checkout for anyone to be in.
func plantUnbornWorktree(t *testing.T, repo, name, wtPath string) {
	t.Helper()
	plantWorktreeDir(t, repo, name, wtPath, map[string]string{
		refWorktreeLockedFile: "initializing",
		refHeadLockFile:       "ref: refs/heads/newborn\n",
	})
}

// plantBornWorktree stages a worktree that finished being created: HEAD
// written, no lock, no `locked` marker.
func plantBornWorktree(t *testing.T, repo, name, wtPath string) {
	t.Helper()
	plantWorktreeDir(t, repo, name, wtPath, map[string]string{
		refHEAD: "ref: refs/heads/born\n",
	})
}

func plantWorktreeDir(t *testing.T, repo, name, wtPath string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(repo, ".git", "worktrees", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files["commondir"] = "../..\n"
	files["gitdir"] = filepath.Join(wtPath, ".git") + "\n"
	for base, body := range files {
		if err := os.WriteFile(filepath.Join(dir, base), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunHookRef_NewWorktreeHeadAdmitted is the bead's first case: two live
// sessions, and the HEAD leg of `git worktree add -b` passes. The worktree it
// belongs to has no working tree yet, so the move rewrites nobody's files.
func TestRunHookRef_NewWorktreeHeadAdmitted(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)
	plantUnbornWorktree(t, repo, "wt2", filepath.Join(t.TempDir(), "wt2"))

	code, stdout, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" ref:refs/heads/z2 "+tcHEAD+"\n")
	if code != 0 {
		t.Fatalf("the new worktree's HEAD must pass: exit %d, %s", code, stderr)
	}
	if !strings.Contains(stdout, "head="+refAdmitWorktreeBirth) {
		t.Errorf("the pass must name the carve-out it took: %q", stdout)
	}
}

// TestRunHookRef_OwnHeadLockedIsNotAWorktreeBirth keeps the carve-out from
// swallowing the case it is next to. `git checkout -b` in the shared checkout
// emits the same row while holding the lock on the checkout's OWN HEAD — that
// is a tree move and stays refused, even with a worktree mid-birth beside it.
func TestRunHookRef_OwnHeadLockedIsNotAWorktreeBirth(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)
	plantUnbornWorktree(t, repo, "wt2", filepath.Join(t.TempDir(), "wt2"))
	if err := os.WriteFile(filepath.Join(repo, ".git", refHeadLockFile), []byte("ref: refs/heads/y2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" ref:refs/heads/y2 "+tcHEAD+"\n")
	if code != 1 {
		t.Fatalf("a HEAD write this checkout itself holds the lock on is a tree move: exit %d, %s", code, stderr)
	}
}

// TestRunHookRef_BornWorktreeDoesNotAdmit: a worktree that finished being
// created is somebody's tree. Its presence admits nothing.
func TestRunHookRef_BornWorktreeDoesNotAdmit(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)
	plantBornWorktree(t, repo, "wt1", filepath.Join(t.TempDir(), "wt1"))

	if code, _, stderr := runRefHook(t, tcPhasePrepared, txnCheckoutOther); code != 1 {
		t.Fatalf("a finished worktree is no licence to move a tree: exit %d, %s", code, stderr)
	}
}

// TestRunHookRef_UnbornWorktreeALivePeerOccupiesStillRefused is the bead's
// first Rule read literally: the admit is "a worktree no live session has
// checked out", not "a worktree dir that looks unfinished".
func TestRunHookRef_UnbornWorktreeALivePeerOccupiesStillRefused(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)
	wtPath := filepath.Join(t.TempDir(), "wt2")
	plantUnbornWorktree(t, repo, "wt2", wtPath)
	plantLiveSessionHere(t, wtPath, "sess-in-wt", "wt-owner-00000001")

	code, _, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" ref:refs/heads/z2 "+tcHEAD+"\n")
	if code != 1 {
		t.Fatalf("a live session in that worktree makes the HEAD write a tree move: exit %d, %s", code, stderr)
	}
}

// TestRunHookRef_UnreadableWorktreeGitdirRefuses: this is the arm that
// WEAKENS the guard, so every unreadable answer is "refuse".
func TestRunHookRef_UnreadableWorktreeGitdirRefuses(t *testing.T) {
	repo := withTempProject(t)
	a := pinAgent(t)
	plantLiveSessionHere(t, repo, "sess-self", a.UUID)
	plantLiveSessionHere(t, repo, "sess-peer", tcPeerOwner)
	plantUnbornWorktree(t, repo, "wt2", filepath.Join(t.TempDir(), "wt2"))
	if err := os.Remove(filepath.Join(repo, ".git", "worktrees", "wt2", "gitdir")); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runRefHook(t, tcPhasePrepared, tcOIDZero+" ref:refs/heads/z2 "+tcHEAD+"\n")
	if code != 1 {
		t.Fatalf("a worktree whose path cannot be read cannot be shown to be empty: exit %d, %s", code, stderr)
	}
}
