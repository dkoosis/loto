package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// settingsHooks mirrors the slice of .claude/settings.json this project ships.
// Only the fields the SessionEnd contract cares about are decoded.
type settingsHooks struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// repoRootFromTest walks up from this test file to the repo root (the dir that
// holds .claude/settings.json). Using runtime.Caller keeps the test independent
// of the working directory `go test` runs in.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root with .claude/settings.json")
		}
		dir = parent
	}
}

func loadSettingsHooks(t *testing.T) settingsHooks {
	t.Helper()
	root := repoRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var s settingsHooks
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return s
}

// TestSessionEndHookReleasesLocks pins the loto-l3as contract: .claude/
// settings.json must register a SessionEnd hook that eagerly releases this
// session's locks via `loto unlock --all` so a clean session exit reclaims
// files immediately instead of waiting out the TTL (README invariant 7).
//
// The crash/kill path deliberately is NOT covered here — that gap is owned by
// pid-liveness + TTL (loto-t1tq), the complementary mechanism.
func TestSessionEndHookReleasesLocks(t *testing.T) {
	s := loadSettingsHooks(t)

	entries, ok := s.Hooks["SessionEnd"]
	if !ok || len(entries) == 0 {
		t.Fatal("settings.json has no SessionEnd hook; clean exit falls back to TTL (loto-l3as)")
	}

	var cmds []string
	for _, e := range entries {
		for _, h := range e.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	joined := strings.Join(cmds, "\n")

	// Must invoke the eager release.
	if !strings.Contains(joined, "loto unlock") || !strings.Contains(joined, "--all") {
		t.Errorf("SessionEnd hook must run `loto unlock --all`; got:\n%s", joined)
	}

	// Must be best-effort: a failing release can never block shutdown.
	if !strings.Contains(joined, "|| true") {
		t.Errorf("SessionEnd hook must be best-effort (|| true) so release never wedges shutdown; got:\n%s", joined)
	}

	// `unlock --all` requires -t (intent) and a pinned LOTO_AGENT_ID. The hook
	// must supply an intent and depend on the SessionStart-exported identity.
	if !strings.Contains(joined, "-t ") && !strings.Contains(joined, "--intent") {
		t.Errorf("SessionEnd hook must pass an intent (-t) — unlock requires it; got:\n%s", joined)
	}
}

// TestSessionStartExportsAgentIDForSessionEnd guards the dependency the
// SessionEnd hook leans on: SessionStart must export LOTO_AGENT_ID (the pin
// that `unlock --all` requires, loto-pody). Without it the SessionEnd release
// would refuse with exit 2 and locks would linger to TTL.
func TestSessionStartExportsAgentIDForSessionEnd(t *testing.T) {
	s := loadSettingsHooks(t)
	entries := s.Hooks["SessionStart"]
	var cmds []string
	for _, e := range entries {
		for _, h := range e.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "LOTO_AGENT_ID") {
		t.Fatalf("SessionStart must export LOTO_AGENT_ID so SessionEnd's `unlock --all` is pinned; got:\n%s", joined)
	}
}
