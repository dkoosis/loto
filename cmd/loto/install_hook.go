package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"loto"
	"loto/internal/render"
)

const (
	writeGateMatcher  = "Edit|Write|NotebookEdit"
	writeGatePreCmd   = "loto hook pre-write"
	writeGatePostCmd  = "loto hook post-write"
	envClaudeSettings = "LOTO_CLAUDE_SETTINGS" // test override
	preToolUse        = "PreToolUse"
	postToolUse       = "PostToolUse"
)

// installHookCmd registers `loto install-hook`.
//
// Default form: writes SessionStart/Stop entries to project .claude/settings.json
// (eager identity-create + release-all-mine on session end).
//
// --write-gate: writes PreToolUse/PostToolUse entries to ~/.claude/settings.json
// that gate Edit|Write|NotebookEdit through 'loto hook pre-write/post-write'.
// Idempotent: re-running detects existing entries by command-string match.
//
// --write-gate --uninstall: removes those entries cleanly.
func installHookCmd() *cobra.Command {
	var writeGate, uninstall bool
	c := &cobra.Command{
		Use:   "install-hook",
		Short: "install Claude Code hooks for loto coordination",
		Long: `Install loto-related hooks into Claude Code settings.

Default (no flags): writes SessionStart + Stop hooks to project-local
.claude/settings.json. SessionStart eager-creates the agent identity;
Stop releases all locks held by this agent.

--write-gate: writes PreToolUse + PostToolUse entries to user-global
~/.claude/settings.json that gate Edit|Write|NotebookEdit through
'loto hook pre-write' and 'loto hook post-write'. Idempotent.

--write-gate --uninstall: removes the write-gate entries. Other hook
entries are preserved; empty hook event arrays are pruned.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if uninstall && !writeGate {
				fmt.Fprintln(os.Stderr, "loto: --uninstall requires --write-gate")
				os.Exit(2)
			}
			if writeGate {
				path := claudeSettingsPath()
				if uninstall {
					changed, err := removeWriteGate(path)
					if err != nil {
						exit(err)
					}
					emitWriteGateUninstall(path, changed)
					return nil
				}
				changed, err := applyWriteGate(path)
				if err != nil {
					exit(err)
				}
				emitWriteGateInstall(path, changed)
				return nil
			}
			if err := writeClaudeHooks(); err != nil {
				exit(err)
			}
			emitInstalled(".claude/settings.json")
			return nil
		},
	}
	c.Flags().BoolVar(&writeGate, "write-gate", false, "install PreToolUse/PostToolUse write-gate in ~/.claude/settings.json")
	c.Flags().BoolVar(&uninstall, "uninstall", false, "remove the write-gate entries (use with --write-gate)")
	return c
}

// claudeSettingsPath returns the user-global Claude settings path.
// LOTO_CLAUDE_SETTINGS overrides for tests.
func claudeSettingsPath() string {
	if v := os.Getenv(envClaudeSettings); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "settings.json")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// readSettings parses a Claude settings file. Missing file → empty map (caller
// will create it). Corrupt JSON returns an error rather than silently truncating.
func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, &loto.ErrSystem{Op: "install-hook: read settings", Err: err}
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, &loto.ErrSystem{Op: "install-hook: parse settings (refusing to truncate)", Err: err}
	}
	if s == nil {
		s = map[string]any{}
	}
	return s, nil
}

// writeSettings serializes settings as pretty JSON and writes to path.
func writeSettings(path string, s map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &loto.ErrSystem{Op: "install-hook: mkdir settings", Err: err}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return &loto.ErrSystem{Op: "install-hook: marshal settings", Err: err}
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306: settings.json is intentionally world-readable
		return &loto.ErrSystem{Op: "install-hook: write settings", Err: err}
	}
	return nil
}

// entryHasCommand reports whether a hook-entry's hooks array contains the given command.
func entryHasCommand(entry any, command string) bool {
	em, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := em[keyHooks].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := hm[keyCommand].(string); c == command {
			return true
		}
	}
	return false
}

// gateEntry returns the standard write-gate entry for a given command.
func gateEntry(command string) map[string]any {
	return map[string]any{
		keyMatcher: writeGateMatcher,
		keyHooks: []any{
			map[string]any{
				keyType:    keyCommand,
				keyCommand: command,
			},
		},
	}
}

// applyWriteGate adds Pre/PostToolUse entries idempotently.
// Returns whether the file was changed.
func applyWriteGate(path string) (bool, error) {
	s, err := readSettings(path)
	if err != nil {
		return false, err
	}
	hooks, ok := s[keyHooks].(map[string]any)
	if !ok {
		hooks = map[string]any{}
	}
	changed := false
	for _, p := range []struct {
		event   string
		command string
	}{
		{preToolUse, writeGatePreCmd},
		{postToolUse, writeGatePostCmd},
	} {
		entries, _ := hooks[p.event].([]any)
		if hasLotoEntry(entries, p.command) {
			continue
		}
		hooks[p.event] = append(entries, gateEntry(p.command))
		changed = true
	}
	if !changed {
		return false, nil
	}
	s[keyHooks] = hooks
	if err := writeSettings(path, s); err != nil {
		return false, err
	}
	return true, nil
}

// hasLotoEntry reports whether any entry's hooks array carries the given command.
func hasLotoEntry(entries []any, command string) bool {
	for _, e := range entries {
		if entryHasCommand(e, command) {
			return true
		}
	}
	return false
}

// removeWriteGate strips entries whose hooks array carries a loto write-gate command.
// Empty event arrays are deleted; an empty hooks map is deleted from settings.
func removeWriteGate(path string) (bool, error) {
	s, err := readSettings(path)
	if err != nil {
		return false, err
	}
	hooks, ok := s[keyHooks].(map[string]any)
	if !ok || hooks == nil {
		return false, nil
	}
	changed := false
	for _, p := range []struct {
		event   string
		command string
	}{
		{preToolUse, writeGatePreCmd},
		{postToolUse, writeGatePostCmd},
	} {
		entries, _ := hooks[p.event].([]any)
		if len(entries) == 0 {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, e := range entries {
			if entryHasCommand(e, p.command) {
				changed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooks, p.event)
		} else {
			hooks[p.event] = kept
		}
	}
	if !changed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(s, keyHooks)
	} else {
		s[keyHooks] = hooks
	}
	return true, writeSettings(path, s)
}

func emitWriteGateInstall(path string, changed bool) {
	if currentFormat == render.FormatLLM {
		if changed {
			_ = render.EmitLLMInstalled(os.Stdout, path)
			fmt.Fprintln(os.Stderr, "→ next: Restart Claude Code. Run loto doctor to verify.")
			return
		}
		_, _ = fmt.Fprintf(os.Stdout, "loto:llm:v1\n✔ already-installed | %s\n", path)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{
		keyInstalled: true,
		keyFile:      path,
		"changed":    changed,
	})
}

func emitWriteGateUninstall(path string, changed bool) {
	if currentFormat == render.FormatLLM {
		token := "uninstalled"
		if !changed {
			token = "not-present"
		}
		_, _ = fmt.Fprintf(os.Stdout, "loto:llm:v1\n✔ %s | %s\n", token, path)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{
		"uninstalled": true,
		keyFile:       path,
		"changed":     changed,
	})
}
