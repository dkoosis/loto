package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

const (
	tcHookCmd  = "hook"
	tcHookPre  = "pre"
	tcHookPost = "post"
	// tcCall41 is §10a test 9's unposted call, named as the spec names it.
	tcCall41 = "call-41"
)

// hookEventJSON builds a harness event. command is written into tool_input
// alongside file_path precisely so the tests can prove loto never reads it —
// pass "" to leave the field out entirely.
func hookEventJSON(tool, toolUseID, filePath, command string) string {
	fields := fmt.Sprintf(`"file_path":%q`, filePath)
	if command != "" {
		fields += fmt.Sprintf(`,"command":%q`, command)
	}
	return fmt.Sprintf(
		`{"session_id":"s-1","tool_name":%q,"tool_use_id":%q,"cwd":"/tmp","tool_input":{%s}}`,
		tool, toolUseID, fields)
}

// hookSubagentEventJSON is hookEventJSON with the harness's per-subagent
// agent_id, the field a /team sibling's call carries and a root call does not.
func hookSubagentEventJSON(tool, toolUseID, filePath, agentID string) string {
	return fmt.Sprintf(
		`{"session_id":"s-1","tool_name":%q,"tool_use_id":%q,"cwd":"/tmp","agent_id":%q,"tool_input":{"file_path":%q}}`,
		tool, toolUseID, agentID, filePath)
}

// mintStampedBeacon runs `loto beacon` as the /team sibling `stamp` would:
// under LOTO_SUBAGENT_ID, which the nested subtest scopes so the caller's own
// hook run stays unstamped, as the harness-spawned hook is.
func mintStampedBeacon(t *testing.T, stamp, target string) {
	t.Helper()
	t.Run("mint stamped beacon "+stamp, func(t *testing.T) {
		t.Setenv("LOTO_SUBAGENT_ID", stamp)
		if code := Run([]string{tcCmdBeacon, target}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("stamped beacon: exit %d", code)
		}
	})
}

// runHookEvent feeds one synthetic event through `loto hook <phase>`.
func runHookEvent(t *testing.T, phase, payload string) (stdout, stderr string, code int) {
	t.Helper()
	prev := hookStdin
	hookStdin = strings.NewReader(payload)
	t.Cleanup(func() { hookStdin = prev })
	var out, errBuf bytes.Buffer
	code = Run([]string{tcHookCmd, phase}, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

// hookStoreRead opens the same store the CLI just wrote to.
func hookStoreRead(t *testing.T) (*runtime, func()) {
	t.Helper()
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("open runtime: %v", err)
	}
	return rt, func() { _ = rt.Close() }
}

func hookCallPaths(t *testing.T, callID string) (store.HookCall, map[string]store.HookCallPath) {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	call, paths, ok, err := rt.Store.CallRecord(rt.Ctx, callID)
	if err != nil {
		t.Fatalf("read call record: %v", err)
	}
	if !ok {
		t.Fatalf("no call record for %s", callID)
	}
	byPath := make(map[string]store.HookCallPath, len(paths))
	for i := range paths {
		byPath[paths[i].Path] = paths[i]
	}
	return call, byPath
}

// The bead's first acceptance criterion, end to end: a synthetic pre then post
// for one Bash call over a checkout with one locked file leaves ONE call
// record carrying seq_pre, seq_post, and the pre and post digest of the locked
// file and of every path git status lists.
func TestHook_PreThenPostRecordsOneCallWithBothDigests(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("seed lock: exit %d", code)
	}

	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-1", "", "")); code != 0 {
		t.Fatalf("pre: exit=%d err=%q", code, errOut)
	}
	// The tool writes. loto never sees the command that did it.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("changed by the tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-1", "", "")); code != 0 {
		t.Fatalf("post: exit=%d err=%q", code, errOut)
	}

	call, paths := hookCallPaths(t, "call-1")
	if call.InFlight() {
		t.Error("a posted call still reads as in flight")
	}
	p, ok := paths[tcTargetA]
	if !ok {
		t.Fatalf("the locked file is not in the record: %v", paths)
	}
	if !p.Locked {
		t.Error("the locked file is not recorded as locked")
	}
	if p.DigestPre == "" || p.DigestPost == "" || p.DigestPre == p.DigestPost {
		t.Errorf("want two different non-empty digests, got pre=%q post=%q", p.DigestPre, p.DigestPost)
	}
	if p.SeqPost != p.SeqPre+1 {
		t.Errorf("a changed path wants the next number: %d -> %d", p.SeqPre, p.SeqPost)
	}
	// Every path git status listed is in the record too, with a digest at both
	// ends — that is what lets a peer judge a change to a path nobody locked.
	if _, ok := paths["internal/store/store.go"]; !ok {
		t.Errorf("a status path is missing from the record: %v", paths)
	}
	for path, rec := range paths {
		if !rec.HasPost {
			t.Errorf("%s has no seq_post", path)
		}
	}
}

// spec §10 tooth 1, and the bead's second criterion: a post carrying the
// tool input's command string, holding a sed in-place edit, is recorded
// IDENTICALLY to one holding `true`. The field is not read, so it cannot
// change an outcome.
func TestHook_ToolInputCommandChangesNothing(t *testing.T) {
	record := func(t *testing.T, command string) map[string]store.HookCallPath {
		t.Helper()
		repo := withTempProject(t)
		pinAgent(t)
		if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("seed lock: exit %d", code)
		}
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-c", "", command)); code != 0 {
			t.Fatalf("pre: exit=%d err=%q", code, errOut)
		}
		if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("rewritten\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-c", "", command)); code != 0 {
			t.Fatalf("post: exit=%d err=%q", code, errOut)
		}
		_, paths := hookCallPaths(t, "call-c")
		return paths
	}

	var withSed, withTrue map[string]store.HookCallPath
	t.Run("sed", func(t *testing.T) { withSed = record(t, `sed -i '' 's/a/b/' `+tcTargetA) })
	t.Run("true", func(t *testing.T) { withTrue = record(t, "true") })

	if len(withSed) != len(withTrue) {
		t.Fatalf("path count differs: sed=%d true=%d", len(withSed), len(withTrue))
	}
	for path, sed := range withSed {
		plain, ok := withTrue[path]
		if !ok {
			t.Errorf("%s recorded only under the sed command", path)
			continue
		}
		if sed.DigestPre != plain.DigestPre || sed.DigestPost != plain.DigestPost ||
			sed.SeqPre != plain.SeqPre || sed.SeqPost != plain.SeqPost || sed.Locked != plain.Locked {
			t.Errorf("%s recorded differently:\n sed  %+v\n true %+v", path, sed, plain)
		}
	}
}

// A repeated post for one tool_use_id changes nothing — no second seq advance,
// no rewritten digest, no moved timestamp.
func TestHook_RepeatedPostIsANoOp(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("seed lock: exit %d", code)
	}
	if _, _, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-r", "", "")); code != 0 {
		t.Fatalf("pre: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-r", "", "")); code != 0 {
		t.Fatalf("first post: exit %d", code)
	}
	callBefore, before := hookCallPaths(t, "call-r")

	// The tree moves again, and the post is replayed. Neither is allowed to
	// rewrite a record whose call is already closed.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-r", "", "")); code != 0 {
		t.Fatalf("repeat post: exit=%d err=%q", code, errOut)
	}
	callAfter, after := hookCallPaths(t, "call-r")
	if !callAfter.TPost.Equal(callBefore.TPost) {
		t.Errorf("t_post moved on the repeat: %v -> %v", callBefore.TPost, callAfter.TPost)
	}
	for path, b := range before {
		if after[path] != b {
			t.Errorf("%s moved on the repeat:\n before %+v\n after  %+v", path, b, after[path])
		}
	}
}

// §10a test 9. Call 41's post never lands; 42 and 43 post inside T_report.
// 41 stays in flight and unflagged — a later pre from the same session never
// closes an earlier call — and past T_report it is post_missing AND still in
// flight.
func TestHook_PostMissingStaysInFlight(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv(hookTReportEnv, "1h")

	for _, id := range []string{tcCall41, "call-42", "call-43"} {
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", id, "", "")); code != 0 {
			t.Fatalf("pre %s: exit=%d err=%q", id, code, errOut)
		}
	}
	for _, id := range []string{"call-42", "call-43"} {
		if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", id, "", "")); code != 0 {
			t.Fatalf("post %s: exit=%d err=%q", id, code, errOut)
		}
	}

	inflight := hookInFlight(t)
	if len(inflight) != 1 || inflight[0].CallID != tcCall41 {
		t.Fatalf("want only call-41 in flight, got %+v", inflight)
	}
	if inflight[0].PostMissing {
		t.Error("call-41 flagged post_missing while inside T_report")
	}

	// Past T_report. The next pre's sweep is what notices.
	t.Setenv(hookTReportEnv, "1ns")
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-44", "", "")); code != 0 {
		t.Fatalf("pre call-44: exit=%d err=%q", code, errOut)
	}
	var seen bool
	for _, c := range hookInFlight(t) {
		if c.CallID != tcCall41 {
			continue
		}
		seen = true
		if !c.PostMissing {
			t.Error("call-41 is past T_report and not flagged post_missing")
		}
		if !c.InFlight() {
			t.Error("post_missing took call-41 out of flight; age never ends a call")
		}
	}
	if !seen {
		t.Error("call-41 left the in-flight set")
	}
}

func hookInFlight(t *testing.T) []store.HookCall {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	calls, err := rt.Store.InFlightCalls(rt.Ctx)
	if err != nil {
		t.Fatalf("in-flight calls: %v", err)
	}
	return calls
}

// I2's three admission cases at the CLI boundary.
func TestHook_EditAdmission(t *testing.T) {
	t.Run("another owner's lock refuses and names the holder", func(t *testing.T) {
		withTempProject(t)
		alice := pinAgent(t)
		if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("alice lock: exit %d", code)
		}
		t.Setenv("LOTO_AGENT_ID", "")
		pinAgent(t) // a second owner in the same checkout

		_, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Edit", "call-refused", tcTargetA, ""))
		if code != 2 {
			t.Fatalf("want exit 2 on a peer's lock, got %d err=%q", code, errOut)
		}
		if !strings.Contains(errOut, alice.UUID) {
			t.Errorf("the refusal must name the holder: %q", errOut)
		}
		// A refused edit changes nothing: no call record either.
		rt, done := hookStoreRead(t)
		defer done()
		if _, _, ok, err := rt.Store.CallRecord(rt.Ctx, "call-refused"); err != nil || ok {
			t.Errorf("a refused pre recorded a call: ok=%v err=%v", ok, err)
		}
	})

	t.Run("an unlocked path is taken and declared", func(t *testing.T) {
		withTempProject(t)
		me := pinAgent(t)
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Edit", "call-take", tcTargetA, "")); code != 0 {
			t.Fatalf("pre: exit=%d err=%q", code, errOut)
		}
		_, paths := hookCallPaths(t, "call-take")
		if !paths[tcTargetA].Declared {
			t.Errorf("the Edit path is not recorded as declared: %+v", paths[tcTargetA])
		}
		rt, done := hookStoreRead(t)
		defer done()
		held, err := rt.Store.ListLocks(rt.Ctx)
		if err != nil {
			t.Fatalf("list locks: %v", err)
		}
		var took bool
		for _, l := range held {
			if l.Target.Canonical == tcTargetA && string(l.OwnerUUID) == me.UUID {
				took = true
			}
		}
		if !took {
			t.Errorf("admission on an unlocked path did not take the lock: %+v", held)
		}
	})

	t.Run("the caller's own lock is admitted", func(t *testing.T) {
		withTempProject(t)
		pinAgent(t)
		if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("lock: exit %d", code)
		}
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Edit", "call-own", tcTargetA, "")); code != 0 {
			t.Fatalf("pre on my own lock: exit=%d err=%q", code, errOut)
		}
		_, paths := hookCallPaths(t, "call-own")
		if !paths[tcTargetA].Declared {
			t.Errorf("my own locked path is not recorded as declared: %+v", paths[tcTargetA])
		}
	})

	// loto-9zcq: the gate script mints a beacon for the same write the pre-hook
	// admits, and nothing orders the two. A beacon is not a write claim — the
	// staged-lock gate does not credit one — so admission over the caller's own
	// beacon must still leave an exclusive lock behind, or the file reaches a
	// commit that nobody holds.
	t.Run("the caller's own beacon is upgraded to an exclusive lock", func(t *testing.T) {
		withTempProject(t)
		me := pinAgent(t)
		if code := Run([]string{tcCmdBeacon, tcTargetA}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("beacon: exit %d", code)
		}
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Write", "call-over-beacon", tcTargetA, "")); code != 0 {
			t.Fatalf("pre over my own beacon: exit=%d err=%q", code, errOut)
		}
		rt, done := hookStoreRead(t)
		defer done()
		held, err := rt.Store.ListLocks(rt.Ctx)
		if err != nil {
			t.Fatalf("list locks: %v", err)
		}
		var exclusive bool
		for _, l := range held {
			if l.Target.Canonical == tcTargetA && string(l.OwnerUUID) == me.UUID && !l.IsBeacon() && l.Mode == domain.ModeExclusive {
				exclusive = true
			}
		}
		if !exclusive {
			t.Errorf("admission over my own beacon left no exclusive lock: %+v", held)
		}
	})

	// loto-0z24: under a /team subagent the gate script mints the beacon with
	// LOTO_SUBAGENT_ID=<agent_id>, so it is owned by a derived sibling, while
	// the hook runs unstamped as the parent. The event's agent_id names that
	// sibling; its beacon is kin for this admission, and the lock the hook
	// takes is still the parent's own.
	t.Run("the current subagent's beacon is kin and is upgraded to the parent's exclusive lock", func(t *testing.T) {
		withTempProject(t)
		me := pinAgent(t)
		mintStampedBeacon(t, tcSubagentA, tcTargetA)
		sib, ok := identity.SubagentOwner(tcSubagentA)
		if !ok || sib == me.UUID {
			t.Fatalf("test premise: the stamp must derive a distinct sibling owner (ok=%v sib=%s me=%s)", ok, sib, me.UUID)
		}
		if _, errOut, code := runHookEvent(t, tcHookPre, hookSubagentEventJSON("Write", "call-sib-beacon", tcTargetA, tcSubagentA)); code != 0 {
			t.Fatalf("pre over my subagent's beacon: exit=%d err=%q", code, errOut)
		}
		rt, done := hookStoreRead(t)
		defer done()
		held, err := rt.Store.ListLocks(rt.Ctx)
		if err != nil {
			t.Fatalf("list locks: %v", err)
		}
		var exclusive, beaconStands bool
		for _, l := range held {
			if l.Target.Canonical == tcTargetA && string(l.OwnerUUID) == me.UUID && !l.IsBeacon() && l.Mode == domain.ModeExclusive {
				exclusive = true
			}
			if l.Target.Canonical == tcTargetA && string(l.OwnerUUID) == sib && l.IsBeacon() {
				beaconStands = true
			}
		}
		if !exclusive {
			t.Errorf("admission over my subagent's beacon left no exclusive lock of mine: %+v", held)
		}
		// The beacon is what a later sibling's stamped `check --gate` refuses
		// on (loto-xwod); admission must leave it standing.
		if !beaconStands {
			t.Errorf("admission retired my subagent's beacon, so siblings no longer serialize on the path: %+v", held)
		}
	})

	t.Run("a sibling's non-beacon row is not authorization: the parent still takes its own lock", func(t *testing.T) {
		withTempProject(t)
		me := pinAgent(t)
		t.Run("stamped shared lock", func(t *testing.T) {
			t.Setenv("LOTO_SUBAGENT_ID", tcSubagentA)
			if code := Run([]string{tcCmdLock, tcFlagShared, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
				t.Fatalf("stamped shared lock: exit %d", code)
			}
		})
		if _, errOut, code := runHookEvent(t, tcHookPre, hookSubagentEventJSON("Edit", "call-sib-shared", tcTargetA, tcSubagentA)); code != 0 {
			t.Fatalf("pre over my subagent's shared lock: exit=%d err=%q", code, errOut)
		}
		rt, done := hookStoreRead(t)
		defer done()
		held, err := rt.Store.ListLocks(rt.Ctx)
		if err != nil {
			t.Fatalf("list locks: %v", err)
		}
		var exclusive bool
		for _, l := range held {
			if l.Target.Canonical == tcTargetA && string(l.OwnerUUID) == me.UUID && l.Mode == domain.ModeExclusive {
				exclusive = true
			}
		}
		if !exclusive {
			t.Errorf("a sibling's shared row was taken as the parent's own authorization; no exclusive lock of mine: %+v", held)
		}
	})

	t.Run("a different sibling's beacon still refuses", func(t *testing.T) {
		withTempProject(t)
		pinAgent(t)
		mintStampedBeacon(t, tcSubagentB, tcTargetA)
		if _, errOut, code := runHookEvent(t, tcHookPre, hookSubagentEventJSON("Write", "call-other-sib", tcTargetA, tcSubagentA)); code != 2 {
			t.Fatalf("pre over a sibling's beacon: exit=%d want 2 err=%q", code, errOut)
		}
	})

	// loto-9zcq, the cause the 2026-09-09 firing actually had: a Write that
	// CREATES its file. The store's target validation refused the missing path
	// for anything but a beacon, hookTakeLock read the error as "no foreign
	// holder" and admitted the write with no lock, and the staged-lock gate
	// then flagged the commit of a file nobody held.
	t.Run("a Write that creates its file takes an exclusive lock", func(t *testing.T) {
		withTempProject(t)
		me := pinAgent(t)
		const created = "created_by_write.go"
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Write", "call-create", created, "")); code != 0 {
			t.Fatalf("pre on a file about to be created: exit=%d err=%q", code, errOut)
		}
		rt, done := hookStoreRead(t)
		defer done()
		held, err := rt.Store.ListLocks(rt.Ctx)
		if err != nil {
			t.Fatalf("list locks: %v", err)
		}
		var exclusive bool
		for _, l := range held {
			if l.Target.Canonical == created && string(l.OwnerUUID) == me.UUID && !l.IsBeacon() && l.Mode == domain.ModeExclusive {
				exclusive = true
			}
		}
		if !exclusive {
			t.Errorf("a Write creating its file was admitted with no exclusive lock: %+v", held)
		}
	})
}

// An unchanged path keeps its number while a changed one advances, in the same
// call — the seq contract stated as a contrast rather than in isolation.
func TestHook_UnchangedPathKeepsItsSeq(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	quiet := "quiet.go"
	if err := os.WriteFile(filepath.Join(repo, quiet), []byte("still\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcTargetA, quiet, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock: exit %d", code)
	}
	if _, _, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-s", "", "")); code != 0 {
		t.Fatalf("pre: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-s", "", "")); code != 0 {
		t.Fatalf("post: exit %d", code)
	}
	_, paths := hookCallPaths(t, "call-s")
	if changed := paths[tcTargetA]; changed.SeqPost != changed.SeqPre+1 {
		t.Errorf("the changed path did not advance: %d -> %d", changed.SeqPre, changed.SeqPost)
	}
	if same := paths[quiet]; same.SeqPost != same.SeqPre {
		t.Errorf("the untouched path advanced: %d -> %d", same.SeqPre, same.SeqPost)
	}
}

// A pre with no tool_use_id, and an unreadable event, get out of the way with
// exit 0. A hook that fails hard on a malformed event disables every tool call
// in the session.
func TestHook_FailsOpenOnAnUnusableEvent(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	for name, payload := range map[string]string{
		"no tool_use_id": `{"session_id":"s","tool_name":"Bash","tool_input":{}}`,
		"not json":       `{`,
	} {
		_, errOut, code := runHookEvent(t, tcHookPre, payload)
		if code != 0 {
			t.Errorf("%s: want exit 0, got %d", name, code)
		}
		if !strings.Contains(errOut, "⚠ hook:") {
			t.Errorf("%s: the skip must say why: %q", name, errOut)
		}
	}
}

// The bead's `loto events` criterion: one hook_timing row per pre and per post.
func TestEvents_ShowsAHookTimingRowPerPreAndPost(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if _, _, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-t", "", "")); code != 0 {
		t.Fatalf("pre: exit %d", code)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "call-t", "", "")); code != 0 {
		t.Fatalf("post: exit %d", code)
	}

	var out, errBuf bytes.Buffer
	if code := Run([]string{"events", "--kind", store.EventHookTiming}, &out, &errBuf); code != 0 {
		t.Fatalf("loto events: exit=%d err=%q", code, errBuf.String())
	}
	body := out.String()
	if !strings.Contains(body, "shown=2") {
		t.Errorf("want two timing rows: %q", body)
	}
	for _, want := range []string{"\tpre\t", "\tpost\t", `"call_id":"call-t"`, `"status_paths":`, `"locked_bytes":`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// An empty read prints a header rather than nothing: silence looks like a
// crash (.claude/rules/design.md).
func TestEvents_EmptyPrintsAHeader(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"events", "--kind", store.EventHookTiming}, &out, &errBuf); code != 0 {
		t.Fatalf("exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "events=0 shown=0") {
		t.Errorf("want an explicit empty header, got %q", out.String())
	}
}

// §3's second ending, end to end: a call whose owner died is no longer in
// flight after the next pre by ANYONE. Without this a crashed session's call
// spans every later transition forever and pins both call tables against
// retention.
func TestHook_DeadOwnersCallIsEndedByTheNextPre(t *testing.T) {
	withTempProject(t)

	// A session whose record names a socket that does not exist reads as DEAD:
	// the session process removes its socket by dying (identity.Verdict).
	dead := pinAgent(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", filepath.Join(t.TempDir(), "gone.sock"))
	if _, err := identity.RecordSession(dead, ""); err != nil {
		t.Fatalf("record the doomed session: %v", err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-crashed", "", "")); code != 0 {
		t.Fatalf("pre: exit=%d err=%q", code, errOut)
	}
	if got := hookInFlight(t); len(got) != 1 || got[0].CallID != "call-crashed" {
		t.Fatalf("want call-crashed in flight before the sweep, got %+v", got)
	}

	// A second owner arrives and runs one call. Its pre sweeps the dead one.
	t.Setenv("LOTO_AGENT_ID", "")
	os.Unsetenv("CLAUDE_CODE_MESSAGING_SOCKET")
	pinAgent(t)
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-next", "", "")); code != 0 {
		t.Fatalf("peer pre: exit=%d err=%q", code, errOut)
	}
	for _, c := range hookInFlight(t) {
		if c.CallID == "call-crashed" {
			t.Fatalf("a dead owner's call is still in flight: %+v", c)
		}
	}
	call, _, ok, err := func() (store.HookCall, []store.HookCallPath, bool, error) {
		rt, done := hookStoreRead(t)
		defer done()
		return rt.Store.CallRecord(rt.Ctx, "call-crashed")
	}()
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if call.DeadAt.IsZero() || !call.TPost.IsZero() {
		t.Errorf("want ended by a dead owner, not by a faked post: %+v", call)
	}
}

// One unhashable path must not cost the whole observation. It is skipped with
// a warning and every other path is still recorded — the pre used to return
// nothing at all, so a single odd name in the tree silently switched
// recording off.
func TestHook_OneUnhashablePathDoesNotSinkTheObservation(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	// A newline in a filename is legal on this filesystem and cannot be fed
	// through `hash-object --stdin-paths`, which reads newline-terminated
	// paths — the deterministic stand-in for every unhashable path.
	oddName := "od\nd.go"
	if err := os.WriteFile(filepath.Join(repo, oddName), []byte("x\n"), 0o644); err != nil {
		t.Skipf("this filesystem rejects a newline in a filename: %v", err)
	}

	_, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "call-odd", "", ""))
	if code != 0 {
		t.Fatalf("pre: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(errOut, "⚠ hook: no digest for") {
		t.Errorf("the skipped path must be named: %q", errOut)
	}
	_, paths := hookCallPaths(t, "call-odd")
	ordinary, ok := paths[tcTargetA]
	if !ok {
		t.Fatalf("the ordinary paths were lost with the odd one: %v", paths)
	}
	if ordinary.DigestPre == "" {
		t.Error("an ordinary path lost its digest to the unhashable one")
	}
}

// TestHookDeliver_NamesTheEventWorktreeWhenDifferent is loto-1h6x's core AC:
// an agent working two linked worktrees of one repo can receive a report at
// whichever one's next pre-hook runs. b.go changes under the owner's own call
// in the linked worktree B; the report that produces is not delivered there
// (hookPost never calls hookDeliver), so it is still waiting when the SAME
// owner's next pre-hook runs in worktree A. Since loto-v6xx a.go in A and a.go
// in B are different files, the row must name B — a bare repo-relative path
// would let the agent inspect or repair the wrong checkout (Codex P2 on
// PR #374).
func TestHookDeliver_NamesTheEventWorktreeWhenDifferent(t *testing.T) {
	repo, wt := siblingCheckouts(t) // cwd is repo (fixer-a); wt is the fixer-b linked worktree
	target := tcTargetB
	if err := os.WriteFile(filepath.Join(wt, target), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if code := Run([]string{tcCmdLock, target, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock in B: exit %d", code)
	}
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "b-1", "", "")); code != 0 {
		t.Fatalf("pre in B: exit=%d err=%q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(wt, target), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "b-1", "", "")); code != 0 {
		t.Fatalf("post in B: exit=%d err=%q", code, errOut)
	}
	evs := treeEvents(t, target)
	if len(evs) != 1 || evs[0].Rule != store.TreeRuleRow2 {
		t.Fatalf("want one row2 event filed in B, got %+v", evs)
	}
	eventWorktree := evs[0].Worktree
	if eventWorktree == "" {
		t.Fatalf("the event carries no worktree, so this test cannot tell A from B")
	}

	// Back to A: the owner's next pre-hook runs in a different worktree than
	// the one the change happened in.
	t.Chdir(repo)
	stdout, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "a-1", "", ""))
	if code != 0 {
		t.Fatalf("pre in A: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(stdout, "reports count=1") {
		t.Fatalf("A's pre must deliver the report filed in B: %q", stdout)
	}
	if !strings.Contains(stdout, "worktree="+eventWorktree) {
		t.Errorf("the delivered row must name B's worktree %q: %q", eventWorktree, stdout)
	}
}

// TestHookDeliver_SameWorktreeRendersUnchanged is loto-1h6x's golden-diff
// half: the same report, delivered at the addressee's next pre-hook in the
// SAME worktree the change happened in, renders exactly as before this bead —
// no worktree= field, since a field that never varies for the common
// single-worktree case is noise (.claude/rules/design.md).
func TestHookDeliver_SameWorktreeRendersUnchanged(t *testing.T) {
	_, wt := siblingCheckouts(t)
	target := tcTargetB
	if err := os.WriteFile(filepath.Join(wt, target), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if code := Run([]string{tcCmdLock, target, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock in B: exit %d", code)
	}
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "b-1", "", "")); code != 0 {
		t.Fatalf("pre in B: exit=%d err=%q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(wt, target), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "b-1", "", "")); code != 0 {
		t.Fatalf("post in B: exit=%d err=%q", code, errOut)
	}

	stdout, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "b-2", "", ""))
	if code != 0 {
		t.Fatalf("second pre in B: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(stdout, "reports count=1") {
		t.Fatalf("B's own next pre must deliver the report: %q", stdout)
	}
	if strings.Contains(stdout, "worktree=") {
		t.Errorf("a same-worktree delivery must render unchanged, no worktree= field: %q", stdout)
	}
}

// TestHook_SteadyStatePreThenPostStaysUnder50msMedian is loto-szdx's
// acceptance criterion, end to end. doctor measured hook_cost p50=123ms /
// p50=141ms in the two busiest repos, both with roughly this many bytes
// under lock (135-285KB) and both dominated by re-hashing locked files that
// had not moved since the call before. 20 locked files summing ~300KB,
// steady state (nothing changes between calls — the case a MEDIAN over
// ongoing use is measuring, not a lock's first call): pre+post together
// must median under 50ms.
func TestHook_SteadyStatePreThenPostStaysUnder50msMedian(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	const nFiles = 20
	const fileSize = 15 * 1024 // 20 * 15KiB ≈ 300KB, matching the bead's benchmark.
	dir := filepath.Join(repo, "bench")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("x"), fileSize)
	targets := make([]string, nFiles)
	for i := range nFiles {
		rel := filepath.Join("bench", fmt.Sprintf("f%02d.go", i))
		if err := os.WriteFile(filepath.Join(repo, rel), content, 0o644); err != nil {
			t.Fatal(err)
		}
		targets[i] = rel
	}

	lockArgs := append([]string{tcCmdLock}, targets...)
	lockArgs = append(lockArgs, tcFlagIntent, tcIntentTest)
	if code := Run(lockArgs, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %d bench files: exit %d", nFiles, code)
	}
	// A file written in the last hookRacyWindow is always hashed; the steady
	// state this measures is files that have sat still longer than that.
	waitPastRacyWindow()

	// Warm the cache: the first pre+post after a lock always hashes (no
	// path_observed row exists yet). Every call after this one is the
	// steady state the median below measures.
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm pre: exit=%d err=%q", code, errOut)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm post: exit=%d err=%q", code, errOut)
	}

	const rounds = 21
	durations := make([]time.Duration, 0, rounds)
	for i := range rounds {
		callID := fmt.Sprintf("steady-%d", i)
		start := time.Now()
		if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", callID, "", "")); code != 0 {
			t.Fatalf("pre %d: exit=%d err=%q", i, code, errOut)
		}
		if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", callID, "", "")); code != 0 {
			t.Fatalf("post %d: exit=%d err=%q", i, code, errOut)
		}
		durations = append(durations, time.Since(start))
	}

	slices.Sort(durations)
	median := durations[len(durations)/2]
	if median >= 50*time.Millisecond {
		t.Errorf("pre+post median = %v over %d rounds, want <50ms (loto-szdx); all: %v", median, rounds, durations)
	}
}

// failHashObject puts a "git" on PATH ahead of the real one that fails ANY
// hash-object call (draining stdin first, so the caller does not block) and
// forwards every other subcommand — status, rev-parse, add, commit — to the
// real binary. It isolates "hashing is unavailable" from "git itself is
// unavailable", the second of which hookObserve's own git-status read cannot
// survive and this test does not want to exercise.
func failHashObject(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("no real git on PATH to wrap: %v", err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"hash-object\" ]; then cat >/dev/null; exit 1; fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHook_LockedUnchangedPathSurvivesHashingBeingUnavailable is loto-szdx's
// deterministic proof, independent of any machine's absolute timing: once a
// locked path's digest is cached in path_observed, a call that observes it
// again with an unmoved stat must not need to hash it at all. git's
// hash-object is made to fail (failHashObject) AFTER the cache is warm and
// the tree is otherwise clean, so a call that still tried to hash would come
// back with an EMPTY digest (hookHashEachPath warns and moves on rather than
// failing the call) — unfixed code re-hashes every locked path on every call
// and cannot produce anything else here; fixed code serves it from cache and
// never touches hash-object.
func TestHook_LockedUnchangedPathSurvivesHashingBeingUnavailable(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	// Commit everything withTempProject seeded so git status is clean — the
	// UNLOCKED half of hookReadPaths always hashes, and this test isolates
	// the locked half the cache covers.
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "seed")

	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock: exit %d", code)
	}
	waitPastRacyWindow()
	// Warm: the first pre+post after a lock always hashes (no path_observed
	// row exists yet), seeding the cache this test then relies on.
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm pre: exit=%d err=%q", code, errOut)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm post: exit=%d err=%q", code, errOut)
	}
	_, warmPaths := hookCallPaths(t, "warm")
	warmDigest := warmPaths[tcTargetA].DigestPost
	if warmDigest == "" {
		t.Fatal("warm call recorded no digest to compare against")
	}

	failHashObject(t)

	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "cold", "", "")); code != 0 {
		t.Fatalf("pre with hashing unavailable: exit=%d err=%q", code, errOut)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "cold", "", "")); code != 0 {
		t.Fatalf("post with hashing unavailable: exit=%d err=%q", code, errOut)
	}

	_, coldPaths := hookCallPaths(t, "cold")
	p, ok := coldPaths[tcTargetA]
	if !ok {
		t.Fatalf("the locked file dropped out of the record with hashing unavailable: %v", coldPaths)
	}
	if p.DigestPre != warmDigest || p.DigestPost != warmDigest {
		t.Errorf("a locked, unchanged path lost its digest when hashing was unavailable: pre=%q post=%q, want %q from cache (loto-szdx)",
			p.DigestPre, p.DigestPost, warmDigest)
	}
}

// rewriteKeepingStat replaces path's bytes with same-size content while
// restoring the mode and the mtime the file had before — the one edit a
// size:mode:mtime fingerprint cannot see (cubic P1 on PR #380). os.Chtimes
// sets mtime from user space; ctime moves with the write and cannot be set
// back, which is what the cache key has to lean on.
func rewriteKeepingStat(t *testing.T, path string, content []byte) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != before.Size() {
		t.Fatalf("rewrite must keep size: have %d, new %d", before.Size(), len(content))
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, before.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if hookStatString(after) != hookStatString(before) {
		t.Fatalf("rewrite moved the size:mode:mtime stat (%q → %q); the test needs it unmoved",
			hookStatString(before), hookStatString(after))
	}
}

// waitPastRacyWindow sleeps until every file touched so far is older than
// hookRacyWindow, so the next observation records a trusted cache key — a
// cache that is never trusted would pass the rewrite test below vacuously.
func waitPastRacyWindow() { time.Sleep(hookRacyWindow + 100*time.Millisecond) }

// TestHook_SameSizeRewriteWithPreservedMtimeIsRehashed is cubic's P1 on PR
// #380 as a red test: a locked file rewritten to different bytes of the same
// size, with its mode and mtime put back, must NOT be served the cached
// digest. The pre that follows must record the new content's digest.
func TestHook_SameSizeRewriteWithPreservedMtimeIsRehashed(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	target := filepath.Join(repo, tcTargetA)
	orig := bytes.Repeat([]byte("a"), 4096)
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "seed")

	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock: exit %d", code)
	}
	waitPastRacyWindow()
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm pre: exit=%d err=%q", code, errOut)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "warm", "", "")); code != 0 {
		t.Fatalf("warm post: exit=%d err=%q", code, errOut)
	}
	_, warmPaths := hookCallPaths(t, "warm")
	warmDigest := warmPaths[tcTargetA].DigestPost
	if warmDigest == "" {
		t.Fatal("warm call recorded no digest")
	}

	changed := bytes.Repeat([]byte("z"), len(orig))
	rewriteKeepingStat(t, target, changed)
	want := strings.TrimSpace(gitOutT(t, repo, "hash-object", tcTargetA))

	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "after", "", "")); code != 0 {
		t.Fatalf("pre after rewrite: exit=%d err=%q", code, errOut)
	}
	_, afterPaths := hookCallPaths(t, "after")
	if got := afterPaths[tcTargetA].DigestPre; got != want {
		t.Errorf("same-size rewrite with preserved mtime: pre digest=%q, want %q (the new bytes); cached %q was served", got, want, warmDigest)
	}
}

// TestHookCacheKey_RacyFileIsNotCached pins the racy rule: a file whose ctime
// is not older than the observation by hookRacyWindow gets no cache key, so a
// same-tick write after the hash cannot hide behind an unmoved ctime. Past
// the window it gets a key that extends the stat string.
func TestHookCacheKey_RacyFileIsNotCached(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if k := hookCacheKey(fi, time.Now()); k != "" {
		t.Errorf("a file written just now got cache key %q, want \"\" (racy)", k)
	}
	k := hookCacheKey(fi, time.Now().Add(hookRacyWindow+time.Second))
	if k == "" {
		t.Fatal("a file older than hookRacyWindow got no cache key")
	}
	if !strings.HasPrefix(k, hookStatString(fi)+":") {
		t.Errorf("cache key %q does not extend the stat string %q", k, hookStatString(fi))
	}
}
