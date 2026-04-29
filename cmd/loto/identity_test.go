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

func TestResolveAgentID_PIDFallback(t *testing.T) {
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("LOTO_CC_PROJECT_DIR", t.TempDir()) // empty dir → no JSONL → fallback

	id, src := resolveAgentID()
	if src != srcPID {
		t.Errorf("src: got %q, want %q", src, srcPID)
	}
	if !regexp.MustCompile(`^pid-\d+$`).MatchString(id) {
		t.Errorf("id %q does not match pid-N", id)
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
