package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"loto"
	"loto/internal/render"
)

// globals set by persistent root flags, available to all subcommands.
var (
	flagBase      string
	flagAgent     string
	flagIntent    string
	flagJSON      bool   // back-compat: synonym for --format=json
	flagFormat    string // "" (auto) | "json" | "llm"
	currentFormat render.Format
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		// Cobra prints the error; we just exit. RunE commands call exit()
		// directly for typed exit codes — this path is for cobra-internal errors.
		os.Exit(2)
	}
}

var rootCmd = &cobra.Command{
	Use:   "loto",
	Short: "lock-out / tag-out coordination for multi-agent workspaces",
	Long: `loto coordinates file and workspace locks across Claude sessions.

Default output is the Claude-Optimized "llm" format (terse, line-oriented)
when stdout is not a tty; tty output is JSON. Override with --format=json|llm
or the legacy --json alias.
Exit codes: 0 success · 1 advisory conflict · 2 usage error · 3 system error`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		explicit := flagFormat
		if flagJSON && explicit == "" {
			explicit = "json"
		}
		currentFormat = render.Resolve(explicit, os.Stdout)
		return nil
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagBase, "base", defaultBase(), "coordination base directory (or $LOTO_BASE)")
	pf.StringVar(&flagAgent, "agent", defaultAgent(), "agent id")
	pf.StringVar(&flagIntent, "intent", "ad-hoc", "human-readable intent")
	pf.BoolVar(&flagJSON, "json", false, "force JSON output (alias for --format=json)")
	pf.StringVar(&flagFormat, "format", "", "output format: json | llm (default: llm when stdout is not a tty)")

	rootCmd.AddCommand(
		tryCmd,
		statusCmd,
		reapCmd,
		breakCmd(),
		whoamiCmd,
		releaseCmd,
		inboxCmd(),
		msgCmd(),
		reserveCmd(),
		installHookCmd,
		checkPathsCmd,
		installGitHookCmd,
		doctorCmd(),
	)
}

// defaultAgent returns LOTO_AGENT_ID if set, otherwise "pid-N".
// The full identity (with handle) is loaded on demand by whoami/currentAgent.
func defaultAgent() string {
	if id := os.Getenv("LOTO_AGENT_ID"); id != "" {
		return id
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

// newLOTO builds a LOTO instance from the current flags, exiting on error.
func newLOTO() *loto.LOTO {
	l, err := loto.New(flagBase)
	if err != nil {
		exit(err)
	}
	return l
}

// ── try ──────────────────────────────────────────────────────────────────────

var tryCmd = &cobra.Command{
	Use:   "try",
	Short: "acquire a lock (non-blocking)",
	Long:  "Acquire a file or global lock. Exits 0 with lock JSON on success, 1 with holder JSON on conflict.",
}

var tryFileHold bool
var tryWait string
var tryTTL string

var tryFileCmd = &cobra.Command{
	Use:   "file <path>",
	Short: "acquire a file lock",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		l := newLOTO()
		target := args[0]
		lock, err := acquireFile(l, flagAgent, flagIntent, target)
		if err != nil {
			exit(err)
		}
		emitTrySuccess("file", target, flagAgent, lock.Conflicts)
		if tryFileHold {
			waitForSignal()
		}
		_ = lock.Unlock()
		return nil
	},
}

var tryGlobalCmd = &cobra.Command{
	Use:   "global",
	Short: "acquire the global lock",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		l := newLOTO()
		lock, err := acquireGlobal(l, flagAgent, flagIntent)
		if err != nil {
			exit(err)
		}
		emitTrySuccess("global", "global", flagAgent, nil)
		if tryFileHold {
			waitForSignal()
		}
		_ = lock.Unlock()
		return nil
	},
}

func init() {
	tryCmd.AddCommand(tryFileCmd, tryGlobalCmd)
	for _, c := range []*cobra.Command{tryFileCmd, tryGlobalCmd} {
		c.Flags().BoolVar(&tryFileHold, "hold", false, "hold lock until SIGINT/SIGTERM (foreground)")
		c.Flags().StringVar(&tryWait, "wait", "", "block until acquired (e.g. 30s, 5m); empty = non-blocking")
		c.Flags().StringVar(&tryTTL, "ttl", "", "advisory expiry on the tag (e.g. 10m, 1h); empty = no expiry")
	}
}

// tagOpts builds a loto.tagOptions from current flag values.
func tagOpts() loto.TagOptions {
	if tryTTL == "" {
		return loto.TagOptions{}
	}
	d, err := time.ParseDuration(tryTTL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto: invalid --ttl duration %q: %v\n", tryTTL, err)
		os.Exit(2)
	}
	return loto.TagOptions{TTL: d}
}

// acquireFile runs TryFileLock or blocks with Acquire depending on --wait.
func acquireFile(l *loto.LOTO, agent, intent, target string) (*loto.ActiveLock, error) {
	opts := tagOpts()
	if tryWait == "" {
		return l.TryFileLock(agent, intent, target, opts)
	}
	d, err := time.ParseDuration(tryWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto: invalid --wait duration %q: %v\n", tryWait, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return l.Acquire(ctx, agent, intent, target, opts)
}

// acquireGlobal runs TryGlobalLock or blocks with AcquireGlobal depending on --wait.
func acquireGlobal(l *loto.LOTO, agent, intent string) (*loto.ActiveLock, error) {
	opts := tagOpts()
	if tryWait == "" {
		return l.TryGlobalLock(agent, intent, opts)
	}
	d, err := time.ParseDuration(tryWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto: invalid --wait duration %q: %v\n", tryWait, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return l.AcquireGlobal(ctx, agent, intent, opts)
}

// ── status ───────────────────────────────────────────────────────────────────

var statusCmd = &cobra.Command{
	Use:   "status [target...]",
	Short: "show lock state for the global lock or given files",
	RunE: func(cmd *cobra.Command, args []string) error {
		l := newLOTO()
		if len(args) == 0 {
			tag, err := l.ReadGlobalTag()
			if err != nil {
				emitStatusGlobal(true, "", "", nil)
				return nil
			}
			emitStatusGlobal(false, tag.AgentID, tag.Intent, tag)
			return nil
		}
		entries := make([]render.StatusEntry, 0, len(args))
		jsonResult := make(map[string]any, len(args))
		for _, t := range args {
			tag, err := l.ReadTag(t)
			if err != nil {
				entries = append(entries, render.StatusEntry{Target: t, Free: true})
				jsonResult[t] = "free"
			} else {
				entries = append(entries, render.StatusEntry{Target: t, Free: false, AgentID: tag.AgentID, Intent: tag.Intent})
				jsonResult[t] = tag
			}
		}
		emitStatusTargets(entries, jsonResult)
		return nil
	},
}

// ── reap ─────────────────────────────────────────────────────────────────────

var reapCmd = &cobra.Command{
	Use:   "reap <path>",
	Short: "remove a stale tag (only when the lock is unheld)",
	Long: `Reap removes a stale tag left by a crashed holder.
Succeeds only if the lock is not currently held (flock is free).
For forced takeover of a live lock, see 'loto break --force' (loto-7wp.19).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		l := newLOTO()
		if err := l.Reap(args[0]); err != nil {
			exit(err)
		}
		emitReaped(args[0])
		return nil
	},
}

// ── break ─────────────────────────────────────────────────────────────────────

func breakCmd() *cobra.Command {
	var force bool
	var reason string

	c := &cobra.Command{
		Use:   "break <path>",
		Short: "remove a stale tag, or force-take a live lock (--force)",
		Long: `Without --force, 'loto break' behaves identically to 'loto reap': it removes
a stale tag only when the lock is not currently held.

With --force, it waits for (or immediately takes) the flock, sends a system
message to the displaced agent's mailbox, then clears the tag. Use --reason
to record why the break was necessary.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			target := args[0]
			if force {
				by := flagAgent
				if err := l.ForceBreak(target, by, reason); err != nil {
					exit(err)
				}
				emitBroken(target, by, reason)
			} else {
				if err := l.Reap(target); err != nil {
					exit(err)
				}
				emitReaped(target)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "force-take a live lock and notify the displaced agent")
	c.Flags().StringVar(&reason, "reason", "", "reason for forced takeover (recorded in displaced agent's mailbox)")
	return c
}

// ── release ───────────────────────────────────────────────────────────────────

var releaseAllMine bool

var releaseCmd = &cobra.Command{
	Use:   "release",
	Short: "release held locks",
	Long:  "Release locks held by this agent. Use --all-mine to release all locks for LOTO_AGENT_ID.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !releaseAllMine {
			fmt.Fprintln(os.Stderr, "loto: release: specify --all-mine (per-handle release coming in a future version)")
			os.Exit(2)
		}
		agent := flagAgent
		if agent == "" || agent == fmt.Sprintf("pid-%d", os.Getpid()) {
			if id := os.Getenv("LOTO_AGENT_ID"); id != "" {
				agent = id
			}
		}
		l := newLOTO()
		released, errs := l.ReleaseAllMine(agent)
		emitReleased(agent, released, errsToStrings(errs))
		if len(errs) > 0 {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	releaseCmd.Flags().BoolVar(&releaseAllMine, "all-mine", false, "release all locks held by this agent (uses LOTO_AGENT_ID)")
}

func errsToStrings(errs []error) []string {
	if len(errs) == 0 {
		return nil
	}
	ss := make([]string, len(errs))
	for i, e := range errs {
		ss[i] = e.Error()
	}
	return ss
}

// ── inbox + msg ───────────────────────────────────────────────────────────────

func inboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inbox <path>",
		Short: "read messages addressed to this agent for a target file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			msgs, err := l.ReadMsgs(args[0], flagAgent)
			if err != nil {
				exit(err)
			}
			emitInbox(args[0], msgs)
			return nil
		},
	}
}

func msgCmd() *cobra.Command {
	var to string
	c := &cobra.Command{
		Use:   "msg <path> <body>",
		Short: "send a message to an agent via a file's mailbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			if err := l.SendMsg(args[0], flagAgent, to, args[1], false); err != nil {
				exit(err)
			}
			emitMsgSent(args[0], to)
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "@all", "recipient agent id or @all")
	return c
}

// ── install-hook ──────────────────────────────────────────────────────────────

var installHookCmd = &cobra.Command{
	Use:   "install-hook",
	Short: "install SessionStart/End hooks for Claude Code",
	Long: `Writes loto hook configuration to .claude/settings.json (project-level).

SessionStart: runs 'loto whoami --ensure' and exports LOTO_AGENT_ID.
SessionStop:  runs 'loto release --all-mine' to clean up this session's locks.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := writeClaudeHooks(); err != nil {
			exit(err)
		}
		emitInstalled(".claude/settings.json")
		return nil
	},
}

func writeClaudeHooks() error {
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		return &loto.ErrSystem{Op: "create .claude dir", Err: err}
	}

	// Read existing settings if present.
	settings := map[string]any{}
	existing, err := os.ReadFile(".claude/settings.json")
	if err == nil {
		_ = json.Unmarshal(existing, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// SessionStart: ensure identity exists and export LOTO_AGENT_ID.
	hooks["SessionStart"] = []any{
		map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "loto whoami --ensure --json | python3 -c \"import sys,json,os; d=json.load(sys.stdin); print('LOTO_AGENT_ID='+d['id'])\" >> $CLAUDE_ENV 2>/dev/null || true",
				},
			},
		},
	}

	// SessionStop: release all locks held by this agent.
	hooks["Stop"] = []any{
		map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "loto release --all-mine --json >/dev/null 2>&1 || true",
				},
			},
		},
	}

	settings["hooks"] = hooks

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return &loto.ErrSystem{Op: "marshal settings", Err: err}
	}
	if err := os.WriteFile(".claude/settings.json", append(data, '\n'), 0o644); err != nil {
		return &loto.ErrSystem{Op: "write settings.json", Err: err}
	}
	return nil
}

// ── whoami ────────────────────────────────────────────────────────────────────

var ensureFlag bool

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "show current agent identity (creates one if LOTO_AGENT_ID is unset)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := currentAgent()
		if err != nil {
			exit(err)
		}
		emitWhoami(a)
		return nil
	},
}

func init() {
	whoamiCmd.Flags().BoolVar(&ensureFlag, "ensure", false, "create identity if missing, then exit 0 (for SessionStart hooks)")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// stubCmd creates a not-yet-implemented subcommand that exits 2.
func stubCmd(use, short, tracker string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short + " [not yet implemented]",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(os.Stderr, "loto: %s: not yet implemented (tracked: %s)\n", use, tracker)
			os.Exit(2)
			return nil
		},
	}
}

// printJSON writes v as indented JSON to stdout. Retained for any callers
// (e.g. helper output paths) that haven't migrated to a per-shape emit*.
func printJSON(v any) {
	_ = render.EmitJSON(os.Stdout, v)
}

// exit writes a typed error to stderr and exits with the appropriate code.
// ErrHeld → exit 1, ErrSystem → exit 3, anything else → exit 1.
func exit(err error) {
	var sys *loto.ErrSystem
	if errors.As(err, &sys) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	var held *loto.ErrHeld
	if errors.As(err, &held) {
		if currentFormat == render.FormatLLM {
			in := render.BlockedInput{Kind: held.Kind, Target: held.Target}
			if held.Tag != nil {
				in.AgentID = held.Tag.AgentID
				in.Intent = held.Tag.Intent
				in.HeldSince = held.Tag.Timestamp
				in.ExpiresAt = held.Tag.ExpiresAt
				in.Branch = held.Tag.Branch
				in.Host = held.Tag.Host
				in.PID = held.Tag.PID
			}
			_ = render.EmitLLMBlocked(os.Stderr, in)
			os.Exit(1)
		}
		// JSON path: ErrHeld.MarshalJSON emits the holder-report shape.
		_ = render.EmitJSON(os.Stderr, held)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// ── format dispatchers ───────────────────────────────────────────────────────

func emitWhoami(a *Agent) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMWhoami(os.Stdout, a.ID, a.Handle, a.Host)
		return
	}
	_ = render.EmitJSON(os.Stdout, a)
}

func emitTrySuccess(kind, target, agent string, warnings []*loto.Reservation) {
	if currentFormat == render.FormatLLM {
		w := make([]render.ReservationWarning, len(warnings))
		for i, r := range warnings {
			w[i] = render.ReservationWarning{Pattern: r.Pattern, AgentID: r.AgentID}
		}
		_ = render.EmitLLMTrySuccess(os.Stdout, kind, target, agent, w)
		return
	}
	var result map[string]any
	if kind == "global" {
		result = map[string]any{"acquired": true, "kind": "global", "agent": agent}
	} else {
		result = map[string]any{"acquired": true, "target": target, "agent": agent}
	}
	if len(warnings) > 0 {
		patterns := make([]string, len(warnings))
		for i, r := range warnings {
			patterns[i] = r.Pattern + " (" + r.AgentID + ")"
		}
		result["reservation_warnings"] = patterns
	}
	_ = render.EmitJSON(os.Stdout, result)
}

func emitStatusGlobal(free bool, agent, intent string, tag *loto.Tag) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMStatusGlobal(os.Stdout, free, agent, intent)
		return
	}
	if free {
		_ = render.EmitJSON(os.Stdout, map[string]any{"global": "free"})
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"global": tag})
}

func emitStatusTargets(entries []render.StatusEntry, jsonResult map[string]any) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMStatusTargets(os.Stdout, entries)
		return
	}
	_ = render.EmitJSON(os.Stdout, jsonResult)
}

func emitInbox(target string, msgs []loto.Msg) {
	if currentFormat == render.FormatLLM {
		out := make([]render.InboxMessage, len(msgs))
		for i, m := range msgs {
			out[i] = render.InboxMessage{From: m.From, To: m.To, Body: m.Body}
		}
		_ = render.EmitLLMInbox(os.Stdout, target, out)
		return
	}
	_ = render.EmitJSON(os.Stdout, msgs)
}

func emitMsgSent(target, to string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMMsgSent(os.Stdout, target, to)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"sent": true, "to": to, "target": target})
}

func emitReleased(agent string, released []string, errs []string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReleased(os.Stdout, agent, len(released), errs)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"agent": agent, "released": released, "errors": errs})
}

func emitReaped(target string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReaped(os.Stdout, target)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"reaped": true, "target": target})
}

func emitBroken(target, by, reason string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMBroken(os.Stdout, target, by, reason)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"broken": true, "force": true, "target": target, "by": by, "reason": reason})
}

func emitInstalled(path string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMInstalled(os.Stdout, path)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"installed": true, "file": path})
}

func waitForSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
}
