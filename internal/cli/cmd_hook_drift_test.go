package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/store"
)

const (
	tcCmdEvents  = "events"
	tcFlagKind   = "--kind"
	tcDriftFile  = "drifted.go"
	tcHookDrift  = "drift"
	tcRuleReport = "tree_change_reported"
)

// undelivered reads every report still waiting, for any addressee.
func undelivered(t *testing.T) []store.TreeReport {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	reports, err := rt.Store.UndeliveredReports(rt.Ctx, "")
	if err != nil {
		t.Fatalf("read reports: %v", err)
	}
	return reports
}

// treeEvents reads every event filed against one path.
func treeEvents(t *testing.T, path string) []store.TreeEvent {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	evs, err := rt.Store.TreeEventsFor(rt.Ctx, path)
	if err != nil {
		t.Fatalf("read tree events: %v", err)
	}
	return evs
}

// eventsOfKind runs `loto events --kind <k>` and returns the row lines.
func eventsOfKind(t *testing.T, kind string) []string {
	t.Helper()
	var out bytes.Buffer
	if code := Run([]string{tcCmdEvents, tcFlagKind, kind}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("loto events --kind %s: exit %d", kind, code)
	}
	var rows []string
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(line, "\t"+kind+"\t") {
			rows = append(rows, line)
		}
	}
	return rows
}

// §10a test 11, end to end. A locked file changed with no call covering it:
// the next pre by anyone reports the drift once, and the post that follows
// does NOT report the same change a second time — step 2 moves observed(f) to
// the digest step 4 is about to record, so the post sees no change at all.
func TestHook_DriftBeforeRecordReportsOnceAndNotAgainAtPost(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcDriftFile), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcDriftFile, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock: exit %d", code)
	}
	// A first call seeds observed(f): nothing is known to differ from yet.
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "seed", "", "")); code != 0 {
		t.Fatalf("seed pre: exit=%d err=%q", code, errOut)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "seed", "", "")); code != 0 {
		t.Fatalf("seed post: exit %d", code)
	}
	if got := len(treeEvents(t, tcDriftFile)); got != 0 {
		t.Fatalf("seeding files no event, got %d", got)
	}

	// Something outside any call changes the locked file.
	if err := os.WriteFile(filepath.Join(repo, tcDriftFile), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "next", "", ""))
	if code != 0 {
		t.Fatalf("pre: exit=%d err=%q", code, errOut)
	}
	evs := treeEvents(t, tcDriftFile)
	if len(evs) != 1 || evs[0].Rule != store.TreeRuleDrift {
		t.Fatalf("want exactly one drift event, got %+v", evs)
	}
	if evs[0].DigestPre == "" || evs[0].DigestPre == evs[0].DigestPost {
		t.Errorf("a drift event carries both digests, got %q -> %q", evs[0].DigestPre, evs[0].DigestPost)
	}
	// The holder is this agent, so its own pre delivers the report at once.
	if !strings.Contains(stdout, "rule="+tcHookDrift) {
		t.Errorf("the drift report was not delivered to the holder: %q", stdout)
	}

	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "next", "", "")); code != 0 {
		t.Fatalf("post: exit %d", code)
	}
	if evs := treeEvents(t, tcDriftFile); len(evs) != 1 {
		t.Errorf("the post re-reported a drift the pre already filed: %+v", evs)
	}
}

// §10a test 16. A call runs `git restore` on an unlocked dirty file, so the
// path LEAVES the status set. An event is filed all the same, carrying the
// dirty digest and the blob hash it went back to. Fail: no event, because the
// path is no longer in `git status`.
func TestHook_PathMadeCleanIsAnEvent(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	tracked := "tracked.go"
	if err := os.WriteFile(filepath.Join(repo, tracked), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, repo)
	if err := os.WriteFile(filepath.Join(repo, tracked), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "restore", "", "")); code != 0 {
		t.Fatalf("pre: exit=%d err=%q", code, errOut)
	}
	// The tool restores the file; loto never sees the command.
	mustGit(t, repo, "restore", tracked)
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "restore", "", "")); code != 0 {
		t.Fatalf("post: exit=%d err=%q", code, errOut)
	}

	evs := treeEvents(t, tracked)
	if len(evs) != 1 {
		t.Fatalf("a path made clean is an event; got %d for %s", len(evs), tracked)
	}
	if evs[0].DigestPre == "" || evs[0].DigestPost == "" || evs[0].DigestPre == evs[0].DigestPost {
		t.Errorf("want d_dirty -> blob hash, got %q -> %q", evs[0].DigestPre, evs[0].DigestPost)
	}
	// Unlocked and uncontested is row 8: the event exists, the report does not
	// (row 8's only output is `L`, which this bead does not build).
	if evs[0].Rule != store.TreeRuleRow8 {
		t.Errorf("an unlocked uncontested change wants row8, got %s", evs[0].Rule)
	}
}

// gitCommitAll makes the fixture's tree a real commit, so `git restore` has a
// HEAD to restore from. The other hook tests never need one.
func gitCommitAll(t *testing.T, repo string) {
	t.Helper()
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "-q", "-m", "fixture")
}

// mustGit is runGit (doctor_binary_test.go) with the failure turned fatal.
func mustGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := runGit(repo, args...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The bead's status criterion: `loto status` lists every undelivered report,
// and a pre by the addressee delivers it and takes it off the list.
func TestStatus_ListsUndeliveredReportsUntilAPreDeliversThem(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	// Alice holds the file. Bob's call writes it — his post files the event
	// and addresses a report to each of them.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock: exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if _, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob pre: exit=%d err=%q", code, errOut)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("bob was here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob post: exit=%d err=%q", code, errOut)
	}

	var out bytes.Buffer
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdStatus}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("status: exit %d", code)
	}
	if !strings.Contains(out.String(), "undelivered count=") {
		t.Fatalf("status must list undelivered reports: %q", out.String())
	}
	if !strings.Contains(out.String(), tcTargetA) {
		t.Errorf("the report must name the path: %q", out.String())
	}
	before := len(undelivered(t))
	if before != 2 {
		t.Fatalf("want one report per addressee, got %d", before)
	}
	// Reading status delivers nothing — it is a read.
	if got := len(undelivered(t)); got != before {
		t.Errorf("status delivered a report: %d -> %d", before, got)
	}

	// Alice's next pre delivers hers, and only hers.
	stdout, errOut, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "alice-1", "", ""))
	if code != 0 {
		t.Fatalf("alice pre: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(stdout, "reports count=1") {
		t.Errorf("alice's pre must hand her report over: %q", stdout)
	}
	left := undelivered(t)
	if len(left) != 1 {
		t.Fatalf("delivery must remove exactly alice's report, got %d left", len(left))
	}
	if string(left[0].Addressee) != bob.UUID {
		t.Errorf("the report left waiting must be bob's, got %s", left[0].Addressee)
	}
	// A second pre by alice repeats nothing.
	stdout, _, _ = runHookEvent(t, tcHookPre, hookEventJSON("Bash", "alice-2", "", ""))
	if strings.Contains(stdout, "reports count=") {
		t.Errorf("a delivered report was handed over twice: %q", stdout)
	}
}

// The bead's events criterion: one tree_change_reported row per report, and a
// tree_change_acted row when the holder's next call puts the reported digest
// back inside the window.
func TestEvents_CarriesTreeChangeReportedAndActed(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	original := []byte("original\n")
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), original, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock: exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if _, _, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob pre: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("bob was here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "bob-1", "", "")); code != 0 {
		t.Fatalf("bob post: exit %d", code)
	}
	if got := len(eventsOfKind(t, tcRuleReport)); got != 2 {
		t.Fatalf("want one tree_change_reported per report, got %d", got)
	}
	if got := len(eventsOfKind(t, "tree_change_acted")); got != 0 {
		t.Fatalf("nobody has acted yet, got %d", got)
	}

	// Alice reads her report, then puts the file back.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if _, _, code := runHookEvent(t, tcHookPre, hookEventJSON("Bash", "alice-1", "", "")); code != 0 {
		t.Fatalf("alice pre: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runHookEvent(t, tcHookPost, hookEventJSON("Bash", "alice-1", "", "")); code != 0 {
		t.Fatalf("alice post: exit %d", code)
	}
	if got := len(eventsOfKind(t, "tree_change_acted")); got != 1 {
		t.Errorf("alice rewrote f back to the reported digest; want one acted row, got %d", got)
	}
}
