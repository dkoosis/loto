package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const (
	testStorePath    = "internal/store/store.go"
	testStorePattern = "internal/store/**"
	testFooBarPath   = "foo/bar.go"
	testAgentBlueOak = "BlueOak"
)

func TestEmitLLMWhoami(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMWhoami(&buf, "2dd46381-9c26-4c01-97ce-91beda0103d1", "RemoteSnipe", "Mac"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header; got:\n%s", got)
	}
	if !strings.Contains(got, "agent | RemoteSnipe | id:2dd46381 | host:Mac\n") {
		t.Fatalf("unexpected body:\n%s", got)
	}
}

func TestEmitLLMTrySuccess(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMTrySuccess(&buf, "file", testStorePath, "GreenCastle", nil); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "✔ acquired | file | internal/store/store.go | by:GreenCastle\n") {
		t.Fatalf("unexpected:\n%s", got)
	}
}

func TestEmitLLMTrySuccessWithReservationWarnings(t *testing.T) {
	var buf bytes.Buffer
	warnings := []ReservationWarning{
		{Pattern: testStorePattern, AgentID: testAgentBlueOak},
	}
	if err := EmitLLMTrySuccess(&buf, "file", testStorePath, "GreenCastle", warnings); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "⚠ reservation | internal/store/** | held-by:BlueOak\n") {
		t.Fatalf("missing warning line:\n%s", got)
	}
}

func TestEmitLLMAcquired(t *testing.T) {
	var buf bytes.Buffer
	expires := time.Date(2026, 5, 9, 14, 30, 0, 0, time.UTC)
	e := AcquireEntry{
		Target:    testStorePath,
		AgentID:   "GreenCastle",
		Intent:    "edit store",
		ExpiresAt: expires,
	}
	if err := EmitLLMAcquired(&buf, e); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header:\n%s", got)
	}
	want := "✔ acquired | internal/store/store.go | by:GreenCastle | intent:edit store | ttl:2026-05-09T14:30:00Z\n"
	if !strings.Contains(got, want) {
		t.Fatalf("unexpected body:\n%s\nwant: %s", got, want)
	}
}

func TestEmitLLMAcquiredWithConflicts(t *testing.T) {
	var buf bytes.Buffer
	e := AcquireEntry{
		Target:    testStorePath,
		AgentID:   "GreenCastle",
		ExpiresAt: time.Date(2026, 5, 9, 14, 30, 0, 0, time.UTC),
		Conflicts: []ReservationWarning{{Pattern: testStorePattern, AgentID: testAgentBlueOak}},
	}
	if err := EmitLLMAcquired(&buf, e); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "⚠ reservation | internal/store/** | held-by:BlueOak\n") {
		t.Fatalf("missing conflict line:\n%s", got)
	}
}

func TestEmitLLMBlocked(t *testing.T) {
	var buf bytes.Buffer
	heldSince := time.Date(2026, 4, 28, 14, 32, 11, 0, time.UTC)
	expires := time.Date(2026, 4, 28, 14, 42, 11, 0, time.UTC)
	in := BlockedInput{
		Kind: "file", Target: testStorePath,
		AgentID: "GreenCastle", Intent: "store refactor",
		HeldSince: heldSince, ExpiresAt: expires,
		Branch: "store-refactor", Host: "dk-mac", PID: 84231,
	}
	if err := EmitLLMBlocked(&buf, in); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := "✗ blocked | file | internal/store/store.go | by:GreenCastle | intent:store refactor | held-since:2026-04-28T14:32:11Z | ttl:2026-04-28T14:42:11Z | branch:store-refactor | host:dk-mac | pid:84231\n"
	if !strings.Contains(got, want) {
		t.Fatalf("blocked line mismatch.\nwant: %q\ngot:  %q", want, got)
	}
}

func TestEmitLLMBlockedTruncatesLongIntent(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("x", 200)
	in := BlockedInput{Kind: "file", Target: "f.go", AgentID: "A", Intent: long, HeldSince: time.Unix(0, 0).UTC()}
	if err := EmitLLMBlocked(&buf, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "intent:"+strings.Repeat("x", 79)+"…") {
		t.Fatalf("intent not truncated; got:\n%s", buf.String())
	}
}

func TestEmitLLMStatusGlobalFree(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMStatusGlobal(&buf, true, "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ global | free\n") {
		t.Fatalf("unexpected:\n%s", buf.String())
	}
}

func TestEmitLLMStatusGlobalHeld(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMStatusGlobal(&buf, false, "GreenCastle", "sweep"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✗ global | by:GreenCastle | intent:sweep\n") {
		t.Fatalf("unexpected:\n%s", buf.String())
	}
}

func TestEmitLLMInboxEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMInbox(&buf, "store.go", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "inbox | store.go | [status: empty]\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMInboxWithMessages(t *testing.T) {
	var buf bytes.Buffer
	msgs := []InboxMessage{
		{From: testAgentBlueOak, To: "@all", Body: "renaming Foo→Bar"},
	}
	if err := EmitLLMInbox(&buf, "store.go", msgs); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "inbox | store.go | 1 msgs\n") {
		t.Fatalf("missing header row:\n%s", got)
	}
	if !strings.Contains(got, "→ from:BlueOak | to:@all | renaming Foo→Bar\n") {
		t.Fatalf("missing msg row:\n%s", got)
	}
}

func TestEmitLLMMsgSent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMMsgSent(&buf, "store.go", testAgentBlueOak); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ msg-sent | target:store.go | to:BlueOak\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMReleased(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReleased(&buf, "GreenCastle", 3, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ released | agent:GreenCastle | n:3\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMReleasedWithErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReleased(&buf, "A", 1, []string{"permission denied"}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "✗ release-error | permission denied\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestEmitLLMReaped(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMReaped(&buf, "store.go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ reaped | store.go\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMBroken(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMBroken(&buf, "store.go", "RedRiver", "stuck"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ broken | store.go | by:RedRiver | reason:stuck\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestEmitLLMInstalled(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMInstalled(&buf, ".claude/settings.json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "✔ installed | .claude/settings.json\n") {
		t.Fatalf("got:\n%s", buf.String())
	}
}

func TestRelPath(t *testing.T) {
	cwd, err := filepathAbs(".")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty passes through", "", ""},
		{"under cwd becomes relative", cwd + "/foo/bar.go", testFooBarPath},
		{"already relative passes through", testFooBarPath, testFooBarPath},
		{"escapes cwd returns input", "/var/tmp/elsewhere", "/var/tmp/elsewhere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RelPath(c.in); got != c.want {
				t.Fatalf("RelPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEmitLLMDoctorTokenHint(t *testing.T) {
	findings := make([]DoctorFinding, tokenHintThreshold+1)
	for i := range findings {
		findings[i] = DoctorFinding{Class: driftStaleTag, Path: "/p/x", Detail: "x"}
	}
	var buf bytes.Buffer
	if err := EmitLLMDoctor(&buf, findings, "check"); err != nil {
		t.Fatal(err)
	}
	want := "# est_tokens:~"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("missing token hint over threshold:\n%s", buf.String())
	}

	// Below threshold: no hint.
	var buf2 bytes.Buffer
	if err := EmitLLMDoctor(&buf2, findings[:1], "check"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), want) {
		t.Fatalf("unexpected token hint at small row count:\n%s", buf2.String())
	}
}

func TestEmitLLMStatusTargets(t *testing.T) {
	var buf bytes.Buffer
	entries := []StatusEntry{
		{Target: "a.go", Free: true},
		{Target: "b.go", Free: false, AgentID: "GreenCastle", Intent: "store refactor"},
	}
	if err := EmitLLMStatusTargets(&buf, entries); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "status | target | holder | intent\n") {
		t.Fatalf("missing column header:\n%s", got)
	}
	if !strings.Contains(got, "✔ free | a.go | - | -\n") {
		t.Fatalf("missing free row:\n%s", got)
	}
	if !strings.Contains(got, "✗ held | b.go | GreenCastle | store refactor\n") {
		t.Fatalf("missing held row:\n%s", got)
	}
}

func TestEmitLLMCheckPathsLockAndReservation(t *testing.T) {
	conflicts := []CheckPathsConflict{
		{Kind: "lock", Path: testStorePath, Holder: testAgentBlueOak, Intent: "refactor"},
		{Kind: checkReservation, Path: testStorePath, Holder: "RedFox",
			Pattern: testStorePattern, Intent: "sweep"},
	}
	var buf bytes.Buffer
	if err := EmitLLMCheckPaths(&buf, conflicts); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header:\n%s", got)
	}
	if !strings.Contains(got, "2 ✗ | check-paths | locks:1 | reservations:1\n") {
		t.Fatalf("missing triage line:\n%s", got)
	}
	if !strings.Contains(got, "✗ lock | internal/store/store.go | by:BlueOak | intent:refactor\n") {
		t.Fatalf("missing lock row:\n%s", got)
	}
	if !strings.Contains(got, "✗ reservation | internal/store/store.go | pattern:internal/store/** | by:RedFox | intent:sweep\n") {
		t.Fatalf("missing reservation row:\n%s", got)
	}
	if !strings.Contains(got, "```bash\nloto break --force internal/store/store.go --reason \"pre-commit\"\n```\n") {
		t.Fatalf("missing lock fix block:\n%s", got)
	}
	if !strings.Contains(got, "```bash\nloto reserve release internal/store/**\n```\n") {
		t.Fatalf("missing reservation fix block:\n%s", got)
	}
}

func TestEmitLLMCheckPathsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMCheckPaths(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "check-paths | [status: ok]\n") {
		t.Fatalf("missing ok line:\n%s", buf.String())
	}
}

func TestEmitLLMDoctorClean(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMDoctor(&buf, nil, "check"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header:\n%s", got)
	}
	if !strings.Contains(got, "doctor | mode:check | [status: ok]\n") {
		t.Fatalf("missing ok line:\n%s", got)
	}
}

func TestEmitLLMDoctorTriageAndFix(t *testing.T) {
	findings := []DoctorFinding{
		{Class: driftStaleTag, Path: "/p/a.tag", AgentID: "Alpha", Detail: "tag without lock"},
		{Class: "soft_stale_held", Path: "/p/b.tag", AgentID: "Bravo", Detail: "TTL expired"},
		{Class: "layout_drift", Path: "/p/junk", Detail: "unexpected entry"},
	}
	var buf bytes.Buffer
	if err := EmitLLMDoctor(&buf, findings, "check"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "1 ✗ 1 ⚠ 1 ℹ | doctor | mode:check\n") {
		t.Fatalf("missing triage line:\n%s", got)
	}
	if !strings.Contains(got, "✗ stale_tag | /p/a.tag | by:Alpha | tag without lock\n") {
		t.Fatalf("missing stale_tag row:\n%s", got)
	}
	if !strings.Contains(got, "⚠ soft_stale_held | /p/b.tag | by:Bravo | TTL expired\n") {
		t.Fatalf("missing soft_stale_held row:\n%s", got)
	}
	if !strings.Contains(got, "ℹ layout_drift | /p/junk | unexpected entry\n") {
		t.Fatalf("missing layout_drift row:\n%s", got)
	}
	// Repairable class gets a fix block; report-only classes do not.
	if !strings.Contains(got, "```bash\nloto doctor --repair\n```\n") {
		t.Fatalf("missing fix block for repairable finding:\n%s", got)
	}
	if strings.Count(got, "loto doctor --repair") != 1 {
		t.Fatalf("fix block should appear exactly once (only stale_tag is repairable):\n%s", got)
	}
}

func TestEmitLLMDoctorRepairStates(t *testing.T) {
	findings := []DoctorFinding{
		{Class: driftStaleTag, Path: "/p/a.tag", Detail: "x", Repaired: true},
		{Class: driftStaleTag, Path: "/p/b.tag", Detail: "x", WouldRepair: true},
		{Class: driftStaleTag, Path: "/p/c.tag", Detail: "x", Error: "perm denied"},
	}
	var buf bytes.Buffer
	if err := EmitLLMDoctor(&buf, findings, "repair"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "| repaired:yes |") {
		t.Fatalf("missing repaired suffix:\n%s", got)
	}
	if !strings.Contains(got, "| would-repair:yes |") {
		t.Fatalf("missing would-repair suffix:\n%s", got)
	}
	if !strings.Contains(got, "| repair-failed:perm denied |") {
		t.Fatalf("missing repair-failed suffix:\n%s", got)
	}
	// Already-repaired and would-repair findings should NOT carry a fix block.
	// Failed-repair should — Claude still has work to do.
	if strings.Count(got, "loto doctor --repair\n```") != 1 {
		t.Fatalf("fix block should appear only for the failed-repair row:\n%s", got)
	}
}

func TestEmitLLMError(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMError(&buf, "create base dir", "permission denied"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "loto:llm:v1\n") {
		t.Fatalf("missing header:\n%s", got)
	}
	if !strings.Contains(got, "✗ error | create base dir | permission denied\n") {
		t.Fatalf("unexpected body:\n%s", got)
	}
}

func TestEmitLLMErrorCollapsesNewlines(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitLLMError(&buf, "op", "line1\nline2"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Count(got, "\n") != 2 { // header + single data line
		t.Fatalf("expected single-line body, got:\n%s", got)
	}
	if !strings.Contains(got, "line1 line2") {
		t.Fatalf("newline not collapsed:\n%s", got)
	}
}
