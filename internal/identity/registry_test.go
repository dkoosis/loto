package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureIdentityCreatesRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == "" || a.Handle == "" {
		t.Fatalf("agent missing fields: %+v", a)
	}
	path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
}

func TestEnsureRespectsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	first, _ := Ensure()
	t.Setenv("LOTO_AGENT_ID", first.UUID)
	second, _ := Ensure()
	if second.UUID != first.UUID {
		t.Fatalf("Ensure() must return same uuid when LOTO_AGENT_ID is set; %s != %s", second.UUID, first.UUID)
	}
}

// TestEnsureDistinctClaudeSessions asserts that two Claude Code sessions on
// the same host resolve to distinct identities. Each Claude session exports a
// unique CLAUDE_CODE_SESSION_ID; Ensure() must consume that signal so that
// concurrent sessions do not collapse onto a shared owner_uuid via the
// mostRecentAgent fallback.
//
// Currently fails — see gh#45 (P0 identity collision). Skip-marker is the
// fix-tracking signal: once Ensure() honors CLAUDE_CODE_SESSION_ID, drop the
// t.Skip call.
func TestEnsureDistinctClaudeSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDECODE", "1")
	// LOTO_AGENT_ID intentionally unset — simulates Claude Bash tool calls
	// where no per-session env-var bridge is configured.
	if _, ok := os.LookupEnv("LOTO_AGENT_ID"); ok {
		t.Setenv("LOTO_AGENT_ID", "")
		os.Unsetenv("LOTO_AGENT_ID")
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-aaaa-1111")
	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-bbbb-2222")
	b, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}

	if a.UUID == b.UUID {
		t.Fatalf("two distinct CLAUDE_CODE_SESSION_ID values produced the same uuid %q — sessions collide via mostRecentAgent fallback (gh#45)", a.UUID)
	}

	// Same session id repeated → same uuid (stable per session).
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-aaaa-1111")
	a2, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if a2.UUID != a.UUID {
		t.Fatalf("same CLAUDE_CODE_SESSION_ID must produce stable uuid; got %s want %s", a2.UUID, a.UUID)
	}
}

// TestWriteAgentAtomic asserts concurrent readers never see partial/empty
// JSON while writeAgent rewrites the same record. Pre-fix (os.WriteFile
// truncate-then-write), readers racing the writer observe zero-byte reads
// or short writes and fail Unmarshal → mostRecentAgent silently drops the
// entry → identity flap (gh#50 / loto-200).
func TestWriteAgentAtomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{}, 2)

	// Writer: rewrite the same record repeatedly.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			select {
			case <-stop:
				return
			default:
				if err := writeAgent(a); err != nil {
					t.Errorf("writeAgent: %v", err)
					return
				}
			}
		}
	}()

	// Reader: read+unmarshal repeatedly. Any partial read is a failure.
	var partial int
	go func() {
		defer func() { done <- struct{}{} }()
		path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
		for range 2000 {
			select {
			case <-stop:
				return
			default:
			}
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var got Agent
			if err := json.Unmarshal(body, &got); err != nil {
				partial++
			}
		}
	}()

	<-done
	close(stop)
	<-done

	if partial > 0 {
		t.Fatalf("writeAgent not atomic: %d partial reads observed", partial)
	}
}

// TestEnsureSessionCachePersists asserts a session cache file is created at
// ~/.loto/session/$SID.json and used on subsequent calls — concurrent calls
// within one Claude session must NOT mint new identities each time
// (loto-aa6 / gh#41).
func TestEnsureSessionCachePersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-cache-test")

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".loto", "session", "session-cache-test.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("session cache not written: %v", err)
	}
	b, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != b.UUID {
		t.Fatalf("session cache not honored: %s != %s", a.UUID, b.UUID)
	}
}

func TestResolveHandleByUUID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	a, _ := Ensure()
	got, err := resolveByHandle(a.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != a.UUID {
		t.Errorf("resolveByHandle: got %s want %s", got.UUID, a.UUID)
	}
}
