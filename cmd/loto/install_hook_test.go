//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	bashTool   = "Bash"
	themeKey   = "theme"
	themeValue = "dark"
)

// readJSON parses a JSON file as a map; t.Fatal on error.
func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return m
}

// hookEvents extracts the entries array for an event from a settings map.
func hookEvents(t *testing.T, settings map[string]any, event string) []any {
	t.Helper()
	hooks, ok := settings[keyHooks].(map[string]any)
	if !ok {
		return nil
	}
	entries, _ := hooks[event].([]any)
	return entries
}

// countLotoEntries returns how many entries in arr carry the given command.
func countLotoEntries(arr []any, command string) int {
	n := 0
	for _, e := range arr {
		if entryHasCommand(e, command) {
			n++
		}
	}
	return n
}

// TestApplyWriteGate_EmptyFile: missing settings.json → file created with both entries.
func TestApplyWriteGate_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	changed, err := applyWriteGate(path)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on first install")
	}
	s := readJSON(t, path)
	if got := countLotoEntries(hookEvents(t, s, preToolUse), writeGatePreCmd); got != 1 {
		t.Errorf("PreToolUse entries: got %d, want 1", got)
	}
	if got := countLotoEntries(hookEvents(t, s, postToolUse), writeGatePostCmd); got != 1 {
		t.Errorf("PostToolUse entries: got %d, want 1", got)
	}
}

// TestApplyWriteGate_Idempotent: re-running yields no duplicates and changed=false.
func TestApplyWriteGate_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := applyWriteGate(path); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	changed, err := applyWriteGate(path)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if changed {
		t.Error("expected changed=false on idempotent reinstall")
	}
	s := readJSON(t, path)
	if got := countLotoEntries(hookEvents(t, s, preToolUse), writeGatePreCmd); got != 1 {
		t.Errorf("PreToolUse entries after reinstall: got %d, want 1", got)
	}
	if got := countLotoEntries(hookEvents(t, s, postToolUse), writeGatePostCmd); got != 1 {
		t.Errorf("PostToolUse entries after reinstall: got %d, want 1", got)
	}
}

// TestApplyWriteGate_PreservesUnrelatedHooks: existing unrelated hooks aren't touched.
func TestApplyWriteGate_PreservesUnrelatedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := map[string]any{
		keyHooks: map[string]any{
			preToolUse: []any{
				map[string]any{
					keyMatcher: bashTool,
					keyHooks: []any{
						map[string]any{keyType: keyCommand, keyCommand: "my-other-hook.sh"},
					},
				},
			},
			"SessionStart": []any{
				map[string]any{keyMatcher: "", keyHooks: []any{
					map[string]any{keyType: keyCommand, keyCommand: "my-session-init.sh"},
				}},
			},
		},
		themeKey: themeValue,
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := applyWriteGate(path); err != nil {
		t.Fatalf("apply: %v", err)
	}
	s := readJSON(t, path)

	// Loto entries present.
	if got := countLotoEntries(hookEvents(t, s, preToolUse), writeGatePreCmd); got != 1 {
		t.Errorf("PreToolUse loto entries: got %d, want 1", got)
	}
	// Original Bash hook still present.
	pre := hookEvents(t, s, preToolUse)
	if len(pre) != 2 {
		t.Errorf("PreToolUse total entries: got %d, want 2", len(pre))
	}
	// SessionStart untouched.
	ss := hookEvents(t, s, "SessionStart")
	if len(ss) != 1 {
		t.Errorf("SessionStart entries: got %d, want 1 (preserved)", len(ss))
	}
	// Top-level theme key preserved.
	if s[themeKey] != themeValue {
		t.Errorf("theme key dropped: %v", s[themeKey])
	}
}

// TestRemoveWriteGate_ClearsEntries: after install + remove, no loto entries remain.
func TestRemoveWriteGate_ClearsEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := applyWriteGate(path); err != nil {
		t.Fatalf("install: %v", err)
	}
	changed, err := removeWriteGate(path)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on first uninstall")
	}
	s := readJSON(t, path)
	if got := countLotoEntries(hookEvents(t, s, preToolUse), writeGatePreCmd); got != 0 {
		t.Errorf("PreToolUse loto entries after uninstall: got %d, want 0", got)
	}
	// Empty event arrays should be deleted entirely.
	hooks, _ := s[keyHooks].(map[string]any)
	if hooks != nil {
		if _, ok := hooks[preToolUse]; ok {
			t.Errorf("PreToolUse key still present after full uninstall")
		}
	}
}

// TestRemoveWriteGate_PreservesUnrelated: removal leaves unrelated hooks intact.
func TestRemoveWriteGate_PreservesUnrelated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := map[string]any{
		keyHooks: map[string]any{
			preToolUse: []any{
				map[string]any{
					keyMatcher: bashTool,
					keyHooks: []any{
						map[string]any{keyType: keyCommand, keyCommand: "my-other-hook.sh"},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := applyWriteGate(path); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := removeWriteGate(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	s := readJSON(t, path)
	pre := hookEvents(t, s, preToolUse)
	if len(pre) != 1 {
		t.Fatalf("PreToolUse entries after uninstall: got %d, want 1 (the unrelated Bash hook)", len(pre))
	}
	em, ok := pre[0].(map[string]any)
	if !ok {
		t.Fatalf("preserved entry not an object: %T", pre[0])
	}
	if em[keyMatcher] != bashTool {
		t.Errorf("preserved entry matcher: got %v, want %s", em[keyMatcher], bashTool)
	}
}

// TestRemoveWriteGate_NoOp: uninstall on a file with no loto entries → changed=false.
func TestRemoveWriteGate_NoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Missing file.
	changed, err := removeWriteGate(path)
	if err != nil {
		t.Fatalf("remove on missing: %v", err)
	}
	if changed {
		t.Error("expected changed=false on missing file")
	}
	// File with unrelated hooks.
	seed := map[string]any{keyHooks: map[string]any{
		preToolUse: []any{
			map[string]any{
				keyMatcher: bashTool,
				keyHooks:   []any{map[string]any{keyType: keyCommand, keyCommand: "x.sh"}},
			},
		},
	}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err = removeWriteGate(path)
	if err != nil {
		t.Fatalf("remove on no-loto file: %v", err)
	}
	if changed {
		t.Error("expected changed=false when no loto entries present")
	}
}

// TestReadSettings_CorruptJSONFailsLoud: corrupt JSON returns error, file untouched.
func TestReadSettings_CorruptJSONFailsLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	corrupt := []byte(`{"hooks": {`) // truncated
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := applyWriteGate(path); err == nil {
		t.Fatal("expected error on corrupt settings; got nil")
	}
	// File contents unchanged.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Errorf("file mutated despite parse error\nbefore: %s\nafter:  %s", corrupt, got)
	}
}

// TestInstallHookCLI_WriteGate: end-to-end via the CLI binary using LOTO_CLAUDE_SETTINGS.
func TestInstallHookCLI_WriteGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Install via CLI.
	cmd := lotoCmd(t.TempDir(), "install-hook", "--write-gate")
	cmd.Env = append(cmd.Env, "LOTO_CLAUDE_SETTINGS="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	s := readJSON(t, path)
	if countLotoEntries(hookEvents(t, s, preToolUse), writeGatePreCmd) != 1 {
		t.Errorf("after CLI install: PreToolUse loto entries != 1")
	}

	// Uninstall via CLI.
	cmd = lotoCmd(t.TempDir(), "install-hook", "--write-gate", "--uninstall")
	cmd.Env = append(cmd.Env, "LOTO_CLAUDE_SETTINGS="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	s = readJSON(t, path)
	hooks, _ := s[keyHooks].(map[string]any)
	if hooks != nil {
		if _, ok := hooks[preToolUse]; ok {
			t.Errorf("PreToolUse still present after CLI uninstall")
		}
	}
}

// TestInstallHookCLI_UninstallRequiresWriteGate: --uninstall alone is a usage error.
func TestInstallHookCLI_UninstallRequiresWriteGate(t *testing.T) {
	cmd := lotoCmd(t.TempDir(), "install-hook", "--uninstall")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected error; got exit 0\n%s", out)
	}
	if !strings.Contains(string(out), "--write-gate") {
		t.Errorf("expected stderr to mention --write-gate; got:\n%s", out)
	}
}
