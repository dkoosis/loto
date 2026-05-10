package main

import (
	"errors"
	"fmt"
	"os"

	"loto"
	"loto/internal/render"
)

// onTimeoutMode selects the policy emitted when a `--wait` deadline elapses
// before a lock is acquired. The default (block) preserves the historical
// "hard timeout" semantics; warn and switch let callers express softer
// policies declaratively instead of parsing exit codes plus stderr text.
type onTimeoutMode int

const (
	onTimeoutBlock onTimeoutMode = iota
	onTimeoutWarn
	onTimeoutSwitch
)

// String values for --on-timeout. Centralized so flag parsing, tests, and
// the JSON `policy` field stay in lockstep.
const (
	policyBlock  = "block"
	policyWarn   = "warn"
	policySwitch = "switch"
)

// parseOnTimeout maps the --on-timeout flag value to a mode. Empty value
// means default (block). Unknown values exit 2 with a usage error so callers
// catch typos at invocation rather than misinterpreting silent fallback.
func parseOnTimeout(s string) onTimeoutMode {
	switch s {
	case "", policyBlock:
		return onTimeoutBlock
	case policyWarn:
		return onTimeoutWarn
	case policySwitch:
		return onTimeoutSwitch
	default:
		fmt.Fprintf(os.Stderr, "loto: invalid --on-timeout %q (want block|warn|switch)\n", s)
		os.Exit(2)
		return 0
	}
}

// emitTimeout writes a structured timeout report and exits with the policy's
// exit code. Centralizes the action/code mapping so try and acquire stay in
// lockstep.
func emitTimeout(held *loto.ErrHeld, mode onTimeoutMode) {
	action, code, policy := timeoutPolicy(mode)
	if currentFormat == render.FormatLLM {
		in := render.BlockedInput{Kind: held.Kind, Target: render.RelPath(held.Target)}
		if held.Tag != nil {
			in.AgentID = displayAgent(held.Tag.AgentID)
			in.Intent = held.Tag.Intent
			in.HeldSince = held.Tag.Timestamp
			in.ExpiresAt = held.Tag.ExpiresAt
			in.Branch = held.Tag.Branch
			in.Host = held.Tag.Host
			in.PID = held.Tag.PID
		}
		_ = render.EmitLLMTimeout(os.Stderr, in, action)
		os.Exit(code)
	}
	payload := map[string]any{
		"timeout":          true,
		"policy":           policy,
		"kind":             held.Kind, //nolint:goconst // payload key shared across emitters
		keyTarget:          held.Target,
		"suggested_action": action,
	}
	if held.Tag != nil {
		payload["blocked_by"] = held.Tag.AgentID
		if held.Tag.Intent != "" {
			payload["intent"] = held.Tag.Intent
		}
	}
	_ = render.EmitJSON(os.Stderr, payload)
	os.Exit(code)
}

// maybeEmitTimeout intercepts an ErrHeld returned from an --wait call (where
// pollAcquire returns the last-observed holder on context expiry) and routes
// it through the on-timeout policy. No-op when wait is empty (non-blocking
// try) or when the error isn't ErrHeld — those flow through exit() as before.
func maybeEmitTimeout(err error, wait string, mode onTimeoutMode) {
	if wait == "" {
		return
	}
	var held *loto.ErrHeld
	if !errors.As(err, &held) {
		return
	}
	emitTimeout(held, mode)
}

func timeoutPolicy(mode onTimeoutMode) (action string, exitCode int, policy string) {
	switch mode {
	case onTimeoutWarn:
		return "proceed", 0, policyWarn
	case onTimeoutSwitch:
		return "msg-and-switch", 1, policySwitch
	case onTimeoutBlock:
		return "abort", 3, policyBlock
	default:
		return "abort", 3, policyBlock
	}
}
