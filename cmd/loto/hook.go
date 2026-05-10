package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"loto"
	"loto/internal/render"
)

const (
	defaultHookWait = 10 * time.Second
	defaultHookTTL  = 5 * time.Minute
	hookPollStart   = 50 * time.Millisecond
	hookPollMax     = 500 * time.Millisecond
	envHookWait     = "LOTO_HOOK_WAIT"
)

// hookCmd returns the `loto hook` parent with pre-write and post-write children.
func hookCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hook",
		Short: "CC PreToolUse/PostToolUse adapters",
		Long: `Subcommands that read Claude Code tool-input JSON from stdin and
call acquire/release. Designed for use in CC hooks settings.json.`,
	}
	c.AddCommand(hookPreWriteCmd(), hookPostWriteCmd())
	return c
}

// ccToolInput is the shape CC passes to PreToolUse/PostToolUse hooks on stdin.
type ccToolInput struct {
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// readHookFilePath parses stdin JSON and returns the file_path.
// Returns ("", false) on bad JSON or missing file_path — callers should exit 0 (fail-safe).
func readHookFilePath(r io.Reader) (string, bool) {
	data, err := io.ReadAll(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto hook: read stdin: %v\n", err)
		return "", false
	}
	var in ccToolInput
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintf(os.Stderr, "loto hook: stdin JSON invalid (allowing): %v\n", err)
		return "", false
	}
	return in.ToolInput.FilePath, in.ToolInput.FilePath != ""
}

// hookWaitDuration returns LOTO_HOOK_WAIT (default 10s).
func hookWaitDuration() time.Duration {
	if v := os.Getenv(envHookWait); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHookWait
}

func hookPreWriteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pre-write",
		Short: "acquire path from CC tool-input JSON (PreToolUse)",
		Long: `Reads CC tool-input JSON from stdin ({tool_input:{file_path:...}}),
acquires a record-tier hold, then exits:
  0  acquired (or file_path absent — not our concern)
  2  conflict persists after LOTO_HOOK_WAIT — CC blocks the write tool

LOTO_HOOK_WAIT sets the max wait (default 10s).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, ok := readHookFilePath(os.Stdin)
			if !ok {
				return nil
			}
			agent := flagAgent
			if agent == "" {
				agent, _ = resolveAgentID()
			}
			l := newLOTO()
			deadline := time.Now().Add(hookWaitDuration())
			interval := hookPollStart
			var lastHeld *loto.ErrHeld
			for {
				_, _, err := l.AcquirePath(agent, flagIntent, path, defaultHookTTL)
				if err == nil {
					return nil
				}
				var held *loto.ErrHeld
				if !errors.As(err, &held) {
					// system error — fail-safe, allow the tool
					fmt.Fprintf(os.Stderr, "loto hook pre-write: system error (allowing): %v\n", err)
					return nil
				}
				lastHeld = held
				if time.Now().After(deadline) {
					emitHookBlocked(path, lastHeld)
					os.Exit(2)
				}
				remaining := time.Until(deadline)
				time.Sleep(min(interval, remaining))
				interval *= 2
				if interval > hookPollMax {
					interval = hookPollMax
				}
			}
		},
	}
}

func hookPostWriteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post-write",
		Short: "release path from CC tool-input JSON (PostToolUse)",
		Long: `Reads CC tool-input JSON from stdin, releases the record-tier hold.
Always exits 0 — post-hook failures must not surface as tool errors.
Robust to pre-write being bypassed: tries release, swallows errors.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, ok := readHookFilePath(os.Stdin)
			if !ok {
				return nil
			}
			agent := flagAgent
			if agent == "" {
				agent, _ = resolveAgentID()
			}
			l := newLOTO()
			_ = l.ReleasePath(agent, path)
			return nil
		},
	}
}

// emitHookBlocked writes a loto:llm:v1 block report to stderr and exits 2.
// CC exit code 2 signals "block this tool call".
func emitHookBlocked(target string, held *loto.ErrHeld) {
	in := render.BlockedInput{
		Kind:   held.Kind,
		Target: render.RelPath(target),
	}
	holderID := "<unknown>"
	if held.Tag != nil {
		in.AgentID = displayAgent(held.Tag.AgentID)
		in.Intent = held.Tag.Intent
		in.HeldSince = held.Tag.Timestamp
		in.ExpiresAt = held.Tag.ExpiresAt
		in.Branch = held.Tag.Branch
		in.Host = held.Tag.Host
		in.PID = held.Tag.PID
		holderID = in.AgentID
	}
	_ = render.EmitLLMBlocked(os.Stderr, in)
	fmt.Fprintf(os.Stderr, "→ next: loto inbox %s  |  loto msg %s --to %s \"need access\"\n",
		render.RelPath(target), render.RelPath(target), holderID)
}
