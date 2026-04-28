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
)

// globals set by persistent root flags, available to all subcommands.
var (
	flagBase   string
	flagAgent  string
	flagIntent string
	flagJSON   bool
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

Outputs structured JSON when --json is set or stdout is not a tty.
Exit codes: 0 success · 1 advisory conflict · 2 usage error · 3 system error`,
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Auto-enable JSON when stdout is not a tty.
		if !flagJSON {
			if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
				flagJSON = true
			}
		}
		return nil
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagBase, "base", defaultBase(), "coordination base directory (or $LOTO_BASE)")
	pf.StringVar(&flagAgent, "agent", defaultAgent(), "agent id")
	pf.StringVar(&flagIntent, "intent", "ad-hoc", "human-readable intent")
	pf.BoolVar(&flagJSON, "json", false, "force JSON output")

	rootCmd.AddCommand(
		tryCmd,
		statusCmd,
		reapCmd,
		breakCmd(),
		whoamiCmd,
		releaseCmd,
		inboxCmd(),
		msgCmd(),
		stubCmd("reserve", "stake an advisory glob reservation", "loto-7wp.23"),
		installHookCmd,
		stubCmd("doctor", "diagnose and optionally repair stale coordination state", "loto-7wp.20"),
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
		if tryFileHold {
			printJSON(map[string]any{"acquired": true, "target": target, "agent": flagAgent})
			waitForSignal()
			_ = lock.Unlock()
			return nil
		}
		printJSON(map[string]any{"acquired": true, "target": target, "agent": flagAgent})
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
		if tryFileHold {
			printJSON(map[string]any{"acquired": true, "kind": "global", "agent": flagAgent})
			waitForSignal()
			_ = lock.Unlock()
			return nil
		}
		printJSON(map[string]any{"acquired": true, "kind": "global", "agent": flagAgent})
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
				printJSON(map[string]any{"global": "free"})
				return nil
			}
			printJSON(map[string]any{"global": tag})
			return nil
		}
		result := make(map[string]any, len(args))
		for _, t := range args {
			tag, err := l.ReadTag(t)
			if err != nil {
				result[t] = "free"
			} else {
				result[t] = tag
			}
		}
		printJSON(result)
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
		printJSON(map[string]any{"reaped": true, "target": args[0]})
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
				printJSON(map[string]any{"broken": true, "force": true, "target": target, "by": by, "reason": reason})
			} else {
				if err := l.Reap(target); err != nil {
					exit(err)
				}
				printJSON(map[string]any{"reaped": true, "target": target})
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
		printJSON(map[string]any{
			"agent":    agent,
			"released": released,
			"errors":   errsToStrings(errs),
		})
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
			printJSON(msgs)
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
			printJSON(map[string]any{"sent": true, "to": to, "target": args[0]})
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
		printJSON(map[string]any{"installed": true, "file": ".claude/settings.json"})
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
		printJSON(a)
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

// printJSON writes v as indented JSON to stdout. Falls back to plain text on error.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// exit writes a typed error to stderr and exits with the appropriate code.
// ErrHeld → exit 1, ErrSystem → exit 3, anything else → exit 1.
func exit(err error) {
	var sys *loto.ErrSystem
	if errors.As(err, &sys) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	// ErrHeld: emit structured JSON to stderr so callers can parse the blocker.
	var held *loto.ErrHeld
	if errors.As(err, &held) && flagJSON {
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(held)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func waitForSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
}
