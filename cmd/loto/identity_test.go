package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// ── resolveAgentID ────────────────────────────────────────────────────────────

func TestResolveAgentID_PrefersLOTOAgentID(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "explicit-handle")
	t.Setenv("LOTO_CC_PROJECT_DIR", "") // prevent accidental JSONL hit

	id, src := resolveAgentID()
	if id != "explicit-handle" {
		t.Errorf("id: got %q, want %q", id, "explicit-handle")
	}
	if src != srcEnv {
		t.Errorf("src: got %q, want %q", src, srcEnv)
	}
}

func TestResolveAgentID_DerivesFromJSONL(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")

	projectDir := t.TempDir()
	t.Setenv("LOTO_CC_PROJECT_DIR", projectDir)

	// Write a fake session JSONL.
	sessionID := "deadbeef-1234-4abc-8abc-abcdef012345"
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	id1, src1 := resolveAgentID()
	id2, src2 := resolveAgentID()

	if id1 != id2 {
		t.Errorf("non-deterministic: %q != %q", id1, id2)
	}
	if src1 != srcSession || src2 != srcSession {
		t.Errorf("src: got %q/%q, want %q", src1, src2, srcSession)
	}
	if !uuidRE.MatchString(id1) {
		t.Errorf("id %q is not UUID-shaped", id1)
	}
}

func TestResolveAgentID_DistinctSessionsDistinctIDs(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")

	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "alpha.jsonl"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "beta.jsonl"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOTO_CC_PROJECT_DIR", dirA)
	idA, _ := resolveAgentID()

	t.Setenv("LOTO_CC_PROJECT_DIR", dirB)
	idB, _ := resolveAgentID()

	if idA == idB {
		t.Errorf("expected distinct IDs for distinct sessions, both were %q", idA)
	}
}

func TestResolveAgentID_ShellFallback(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("LOTO_CC_PROJECT_DIR", t.TempDir()) // empty dir → no JSONL → fallback

	id, src := resolveAgentID()
	if src != srcPID {
		t.Errorf("src: got %q, want %q", src, srcPID)
	}
	if !regexp.MustCompile(`^shell-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("id %q does not match shell-<hex>", id)
	}
}

// TestShellAgentID_StableAcrossCalls: a non-CC shell must produce the same
// agent ID across multiple loto invocations from the same parent shell +
// CWD. This is the loto-edt regression: every invocation used to mint a
// fresh pid-N, accumulating zombie agents.
func TestShellAgentID_StableAcrossCalls(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("LOTO_CC_PROJECT_DIR", t.TempDir())

	id1, _ := resolveAgentID()
	id2, _ := resolveAgentID()
	if id1 != id2 {
		t.Errorf("non-deterministic shell ID: %q != %q", id1, id2)
	}
}

// ── discoverCCSessionIDFrom ───────────────────────────────────────────────────

func TestDiscoverCCSessionIDFrom_Empty(t *testing.T) {
	dir := t.TempDir()
	if got := discoverCCSessionIDFrom(dir); got != "" {
		t.Errorf("expected empty on empty dir, got %q", got)
	}
}

func TestDiscoverCCSessionIDFrom_MissingDir(t *testing.T) {
	if got := discoverCCSessionIDFrom("/nonexistent/path/xyz"); got != "" {
		t.Errorf("expected empty on missing dir, got %q", got)
	}
}

func TestDiscoverCCSessionIDFrom_SingleFile(t *testing.T) {
	dir := t.TempDir()
	want := "abc123session"
	if err := os.WriteFile(filepath.Join(dir, want+".jsonl"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := discoverCCSessionIDFrom(dir); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDiscoverCCSessionIDFrom_PicksNewest(t *testing.T) {
	dir := t.TempDir()

	// Write older file.
	old := filepath.Join(dir, "old-session.jsonl")
	if err := os.WriteFile(old, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate it.
	past := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	// Write newer file.
	if err := os.WriteFile(filepath.Join(dir, "new-session.jsonl"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := discoverCCSessionIDFrom(dir); got != "new-session" {
		t.Errorf("got %q, want %q", got, "new-session")
	}
}

func TestDiscoverCCSessionIDFrom_IgnoresNonJSONL(t *testing.T) {
	dir := t.TempDir()
	// Write non-.jsonl files.
	for _, name := range []string{"foo.txt", "bar.json", "baz"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := discoverCCSessionIDFrom(dir); got != "" {
		t.Errorf("expected empty when no .jsonl files, got %q", got)
	}
}

// ── sessionUUID ───────────────────────────────────────────────────────────────

func TestSessionUUID_Deterministic(t *testing.T) {
	a := sessionUUID("session-abc")
	b := sessionUUID("session-abc")
	if a != b {
		t.Errorf("non-deterministic: %q != %q", a, b)
	}
}

func TestSessionUUID_UUIDShaped(t *testing.T) {
	id := sessionUUID("session-abc")
	if !uuidRE.MatchString(id) {
		t.Errorf("sessionUUID %q is not UUID-shaped", id)
	}
}

func TestSessionUUID_DistinctInputs(t *testing.T) {
	if sessionUUID("alpha") == sessionUUID("beta") {
		t.Error("expected distinct UUIDs for distinct session IDs")
	}
}

// ── validateHandle ────────────────────────────────────────────────────────────

func TestValidateHandle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"slash", "foo/bar", false},
		{"backslash", "foo\\bar", false},
		{"control-tab", "foo\tbar", false},
		{"control-null", "foo\x00bar", false},
		{"too-long", "abcdefghijabcdefghijabcdefghijabc", false}, // 33 chars
		{"max-length", "abcdefghijabcdefghijabcdefghijab", true}, // 32 chars
		{"simple", "Scout", true},
		{"with-digit", "Scout2", true},
		{"with-dash", "blue-oak", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHandle(tc.in)
			if tc.ok && err != nil {
				t.Errorf("validateHandle(%q) = %v, want nil", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateHandle(%q) = nil, want error", tc.in)
			}
		})
	}
}

// ── setHandle ────────────────────────────────────────────────────────────────

func TestSetHandle_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOTO_AGENT_ID", "test-agent-id")
	t.Setenv("LOTO_CC_PROJECT_DIR", "")

	if _, err := setHandle("test-agent-id", "Scout"); err != nil {
		t.Fatalf("setHandle: %v", err)
	}

	a, err := currentAgent()
	if err != nil {
		t.Fatalf("currentAgent: %v", err)
	}
	if a.Handle != "Scout" {
		t.Errorf("Handle = %q, want %q", a.Handle, "Scout")
	}
	if a.ID != "test-agent-id" {
		t.Errorf("ID = %q, want %q", a.ID, "test-agent-id")
	}
}

func TestSetHandle_CreatesIfMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a, err := setHandle("brand-new-id", "Newbie")
	if err != nil {
		t.Fatalf("setHandle: %v", err)
	}
	if a.Handle != "Newbie" {
		t.Errorf("Handle = %q, want Newbie", a.Handle)
	}
	if a.ID != "brand-new-id" {
		t.Errorf("ID = %q, want brand-new-id", a.ID)
	}

	// Verify on-disk presence.
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".loto", "agents", "brand-new-id.json")); err != nil {
		t.Errorf("agent file not written: %v", err)
	}
}

func TestSetHandle_UpdatesExisting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOTO_AGENT_ID", "stable-id")
	t.Setenv("LOTO_CC_PROJECT_DIR", "")

	first, err := currentAgent()
	if err != nil {
		t.Fatalf("currentAgent: %v", err)
	}
	originalCreated := first.CreatedAt

	a, err := setHandle("stable-id", "Renamed")
	if err != nil {
		t.Fatalf("setHandle: %v", err)
	}
	if a.Handle != "Renamed" {
		t.Errorf("Handle = %q, want Renamed", a.Handle)
	}
	// CreatedAt should be preserved (not reset to now).
	if !a.CreatedAt.Equal(originalCreated) {
		t.Errorf("CreatedAt mutated: was %v, now %v", originalCreated, a.CreatedAt)
	}
}

func TestSetHandle_RejectsInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := setHandle("any-id", "foo/bar"); err == nil {
		t.Error("expected error for invalid handle, got nil")
	}
}
