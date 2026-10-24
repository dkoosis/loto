package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClaudeSessionsMissingDirIsEmpty: no ~/.claude/sessions is no sessions,
// not an error.
func TestClaudeSessionsMissingDirIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ClaudeSessions()
	if err != nil || got != nil {
		t.Fatalf("ClaudeSessions() = %v, %v; want nil, nil", got, err)
	}
}

// TestClaudeSessionsUnreadableDirErrors: a sessions path that exists but
// cannot be listed returns an error, so check --worktree reports unknown
// instead of reading an empty list as free.
func TestClaudeSessionsUnreadableDirErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaudeSessions(); err == nil {
		t.Fatal("ClaudeSessions() on a non-directory: want an error, got nil")
	}
}
