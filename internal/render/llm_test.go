package render

import (
	"bytes"
	"strings"
	"testing"
	"time"
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
	if err := EmitLLMTrySuccess(&buf, "file", "internal/store/store.go", "GreenCastle", nil); err != nil {
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
		{Pattern: "internal/store/**", AgentID: "BlueOak"},
	}
	if err := EmitLLMTrySuccess(&buf, "file", "internal/store/store.go", "GreenCastle", warnings); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "⚠ reservation | internal/store/** | held-by:BlueOak\n") {
		t.Fatalf("missing warning line:\n%s", got)
	}
}

func TestEmitLLMBlocked(t *testing.T) {
	var buf bytes.Buffer
	heldSince := time.Date(2026, 4, 28, 14, 32, 11, 0, time.UTC)
	expires := time.Date(2026, 4, 28, 14, 42, 11, 0, time.UTC)
	in := BlockedInput{
		Kind: "file", Target: "internal/store/store.go",
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
		{From: "BlueOak", To: "@all", Body: "renaming Foo→Bar"},
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
	if err := EmitLLMMsgSent(&buf, "store.go", "BlueOak"); err != nil {
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
