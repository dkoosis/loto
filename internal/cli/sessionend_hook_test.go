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

// commandsForEvent flattens every hook command registered for the given
// lifecycle event into one newline-joined string for substring assertions.
func commandsForEvent(s settingsHooks, event string) string {
	var cmds []string
	for _, e := range s.Hooks[event] {
		for _, h := range e.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	return strings.Join(cmds, "\n")
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

	if len(s.Hooks["SessionEnd"]) == 0 {
		t.Fatal("settings.json has no SessionEnd hook; clean exit falls back to TTL (loto-l3as)")
	}
	joined := commandsForEvent(s, "SessionEnd")

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

// TestSessionStartExportsSessionIDForSessionEnd guards the dependency the
// SessionEnd hook leans on: SessionStart must carry the owner pin that
// `unlock --all` requires (loto-pody) into later hook shells. Without it the
// SessionEnd release would refuse with exit 2 and locks would linger to TTL.
//
// The pin is CLAUDE_CODE_SESSION_ID, published through $CLAUDE_ENV, NOT a
// LOTO_AGENT_ID derived from `whoami` (loto-jnid): since the owner id IS the
// session id, that export only re-routed the value into the STRICT
// canonical-hex leg, which the session leg deliberately does not enforce — a
// non-hex session id would then fail every write verb.
//
// The value is read from the hook's stdin JSON (`session_id`), falling back to
// the environment. Claude Code's hook reference documents the stdin field; it
// does not document exporting the variable into hook command environments, so
// stdin is the source that is guaranteed to be there.
func TestSessionStartExportsSessionIDForSessionEnd(t *testing.T) {
	s := loadSettingsHooks(t)
	joined := commandsForEvent(s, "SessionStart")
	if !strings.Contains(joined, "CLAUDE_CODE_SESSION_ID") {
		t.Fatalf("SessionStart must carry CLAUDE_CODE_SESSION_ID so SessionEnd's `unlock --all` is pinned; got:\n%s", joined)
	}
	// It must come off the hook's stdin payload, not only off the hook's own
	// environment: Claude Code documents session_id as a stdin JSON field and
	// does NOT document exporting CLAUDE_CODE_SESSION_ID into hook commands,
	// so an env-only read is unpinned on any build that does not export it.
	if !strings.Contains(joined, "session_id") {
		t.Errorf("SessionStart must read session_id from the hook's stdin JSON, not env alone; got:\n%s", joined)
	}
	if strings.Contains(joined, "LOTO_AGENT_ID") {
		t.Errorf("SessionStart must NOT export LOTO_AGENT_ID: the owner id is the session id, and that export forces it through the strict-hex leg; got:\n%s", joined)
	}
}
