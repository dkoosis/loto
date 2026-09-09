package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
