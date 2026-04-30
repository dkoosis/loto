// Command loto is the CLI for lock-out/tag-out coordination across concurrent
// Claude sessions — claims file and global locks, tags them with agent
// identity, and surfaces holders so editors don't silently clobber each other.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

var errInvalidImportance = errors.New("--importance must be one of: low, normal, urgent")

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

// defaultAgent returns the stable session-scoped agent ID via resolveAgentID
// (LOTO_AGENT_ID → CC session JSONL → "pid-N"). The full identity record
// (with handle) is loaded on demand by whoami/currentAgent.
func defaultAgent() string {
	id, _ := resolveAgentID()
	return id
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

// waitContext parses --wait and returns a bounded context, or (nil, nil) when
// --wait is empty (non-blocking). On parse error it exits 2.
func waitContext() (context.Context, context.CancelFunc) {
	if tryWait == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(tryWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto: invalid --wait duration %q: %v\n", tryWait, err)
		os.Exit(2)
	}
	return context.WithTimeout(context.Background(), d)
}

// acquireFile runs TryFileLock or blocks with Acquire depending on --wait.
func acquireFile(l *loto.LOTO, agent, intent, target string) (*loto.ActiveLock, error) {
	opts := tagOpts()
	ctx, cancel := waitContext()
	if ctx == nil {
		return l.TryFileLock(agent, intent, target, opts)
	}
	defer cancel()
	return l.Acquire(ctx, agent, intent, target, opts)
}

// acquireGlobal runs TryGlobalLock or blocks with AcquireGlobal depending on --wait.
func acquireGlobal(l *loto.LOTO, agent, intent string) (*loto.ActiveLock, error) {
	opts := tagOpts()
	ctx, cancel := waitContext()
	if ctx == nil {
		return l.TryGlobalLock(agent, intent, opts)
	}
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
		if agent == "" {
			agent, _ = resolveAgentID()
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
	var (
		mine         bool
		since        string
		noCheckpoint bool
	)
	c := &cobra.Command{
		Use:   "inbox [path]",
		Short: "read mailbox: per-file (path) or cross-file (--mine)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			l := newLOTO()
			if mine {
				if len(args) > 0 {
					return errInboxMineArg
				}
				return runInboxMine(l, since, noCheckpoint)
			}
			if len(args) != 1 {
				return errInboxNeedsPath
			}
			msgs, err := l.ReadMsgs(args[0], flagAgent)
			if err != nil {
				exit(err)
			}
			emitInbox(args[0], msgs)
			return nil
		},
	}
	c.Flags().BoolVar(&mine, "mine", false, "read all mailboxes addressed to this agent")
	c.Flags().StringVar(&since, "since", "", "RFC3339 timestamp; overrides the per-agent checkpoint")
	c.Flags().BoolVar(&noCheckpoint, "no-checkpoint", false, "do not advance the per-agent checkpoint after reading")
	return c
}

func runInboxMine(l *loto.LOTO, sinceFlag string, noCheckpoint bool) error {
	var since time.Time
	switch {
	case sinceFlag != "":
		t, err := time.Parse(time.RFC3339, sinceFlag)
		if err != nil {
			return fmt.Errorf("loto: invalid --since %q: %w", sinceFlag, err)
		}
		since = t
	default:
		if t, ok := readInboxCheckpoint(flagBase, flagAgent); ok {
			since = t
		}
	}
	msgs, err := l.ReadAllMsgs(flagAgent, since)
	if err != nil {
		exit(err)
	}
	emitInboxMine(msgs, since)
	if !noCheckpoint && sinceFlag == "" {
		// Advance checkpoint to max(seen ts) so the next call returns only
		// strictly newer messages. Empty result preserves the prior checkpoint.
		var newest time.Time
		for _, m := range msgs {
			if m.Timestamp.After(newest) {
				newest = m.Timestamp
			}
		}
		if !newest.IsZero() {
			// +1ns so the next read excludes messages we just saw without
			// dropping a sibling that shares the same nanosecond stamp.
			_ = writeInboxCheckpoint(flagBase, flagAgent, newest.Add(time.Nanosecond))
		}
	}
	return nil
}

func inboxCheckpointPath(base, agentID string) string {
	return filepath.Join(base, "agents", agentID+".checkpoint")
}

func readInboxCheckpoint(base, agentID string) (time.Time, bool) {
	data, err := os.ReadFile(inboxCheckpointPath(base, agentID))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func writeInboxCheckpoint(base, agentID string, t time.Time) error {
	dir := filepath.Join(base, "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(inboxCheckpointPath(base, agentID),
		[]byte(t.UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

var (
	errInboxMineArg   = errors.New("loto: inbox --mine takes no path argument")
	errInboxNeedsPath = errors.New("loto: inbox needs a path (or use --mine)")
)

func msgCmd() *cobra.Command {
	var (
		to         string
		ack        bool
		importance string
	)
	c := &cobra.Command{
		Use:   "msg <path> <body>",
		Short: "send a message to an agent via a file's mailbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch importance {
			case "", loto.ImportanceLow, loto.ImportanceNormal, loto.ImportanceUrgent:
			default:
				return errInvalidImportance
			}
			l := newLOTO()
			err := l.SendMsgWith(args[0], loto.Msg{
				From:        flagAgent,
				To:          to,
				Body:        args[1],
				AckRequired: ack,
				Importance:  importance,
			})
			if err != nil {
				exit(err)
			}
			emitMsgSent(args[0], to)
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "@all", "recipient agent id or @all")
	c.Flags().BoolVar(&ack, "ack", false, "request ACK — recipient's first read stamps read_at")
	c.Flags().StringVar(&importance, "importance", "", "advisory hint: low|normal|urgent (default normal)")
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

	// SessionStart: eager-create the agent file so the first loto invocation
	// finds it instantly. Identity is recovered on demand from the CC session
	// JSONL, so no env-var injection is required.
	hooks["SessionStart"] = []any{
		map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": "loto whoami --ensure >/dev/null 2>&1 || true",
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
	if err := os.WriteFile(".claude/settings.json", append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306: project-local config, world-readable by design
		return &loto.ErrSystem{Op: "write settings.json", Err: err}
	}
	return nil
}

// ── whoami ────────────────────────────────────────────────────────────────────

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
	// --ensure is accepted for SessionStart-hook compatibility; whoami already
	// creates identity on demand, so the flag is a no-op (kept to avoid breaking
	// existing hook scripts).
	whoamiCmd.Flags().Bool("ensure", false, "create identity if missing, then exit 0 (for SessionStart hooks)")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// printJSON writes v as indented JSON to stdout. Used by commands whose
// shape doesn't warrant a dedicated render.EmitLLM* helper (doctor report,
// check-paths conflict list, install-git-hook ack).
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
				in.AgentID = displayAgent(held.Tag.AgentID)
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
			w[i] = render.ReservationWarning{Pattern: r.Pattern, AgentID: displayAgent(r.AgentID)}
		}
		_ = render.EmitLLMTrySuccess(os.Stdout, kind, target, displayAgent(agent), w)
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
		_ = render.EmitLLMStatusGlobal(os.Stdout, free, displayAgent(agent), intent)
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
		display := make([]render.StatusEntry, len(entries))
		for i, e := range entries {
			display[i] = e
			display[i].AgentID = displayAgent(e.AgentID)
		}
		_ = render.EmitLLMStatusTargets(os.Stdout, display)
		return
	}
	_ = render.EmitJSON(os.Stdout, jsonResult)
}

func emitInboxMine(msgs []loto.Msg, since time.Time) {
	if currentFormat == render.FormatLLM {
		out := make([]render.InboxMineMessage, len(msgs))
		for i, m := range msgs {
			out[i] = render.InboxMineMessage{
				From:      displayAgent(m.From),
				To:        displayAgent(m.To),
				Target:    m.Target,
				Timestamp: m.Timestamp,
				Body:      m.Body,
			}
		}
		_ = render.EmitLLMInboxMine(os.Stdout, out, since)
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{
		"messages": msgs,
		"since":    sinceJSON(since),
	})
}

func sinceJSON(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func emitInbox(target string, msgs []loto.Msg) {
	if currentFormat == render.FormatLLM {
		out := make([]render.InboxMessage, len(msgs))
		for i, m := range msgs {
			out[i] = render.InboxMessage{From: displayAgent(m.From), To: displayAgent(m.To), Body: m.Body}
		}
		_ = render.EmitLLMInbox(os.Stdout, target, out)
		return
	}
	_ = render.EmitJSON(os.Stdout, msgs)
}

func emitMsgSent(target, to string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMMsgSent(os.Stdout, target, displayAgent(to))
		return
	}
	_ = render.EmitJSON(os.Stdout, map[string]any{"sent": true, "to": to, "target": target})
}

func emitReleased(agent string, released []string, errs []string) {
	if currentFormat == render.FormatLLM {
		_ = render.EmitLLMReleased(os.Stdout, displayAgent(agent), len(released), errs)
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
		_ = render.EmitLLMBroken(os.Stdout, target, displayAgent(by), reason)
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
