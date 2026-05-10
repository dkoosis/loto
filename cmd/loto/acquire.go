package main

import (
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"loto"
	"loto/internal/render"
)

const (
	defaultAcquireTTL = 5 * time.Minute

	// Inline polling constants for `acquire --wait`. Kept local per Step 12's
	// constraint not to refactor the library-side pollAcquire (which polls
	// flock-tier locks and returns *ActiveLock; AcquirePath returns *Tag +
	// reservations, so reuse would require generics or a cast layer for one
	// caller). Smaller starting interval than pollAcquire's 200ms because
	// CLI --wait values tend to be short (sub-second to seconds).
	acquirePollStart = 50 * time.Millisecond
	acquirePollMax   = 500 * time.Millisecond
)

// acquireCmd registers `loto acquire <path>` — the record-tier blocking
// hold introduced by loto-ux3.1. Default TTL is 5m. --wait polls until
// the holder releases or the timer expires; exit 3 distinguishes "ran
// out of wait time" from immediate conflict (exit 1).
func acquireCmd() *cobra.Command {
	var ttl, wait string
	c := &cobra.Command{
		Use:   "acquire <path>",
		Short: "record-tier hold on a path (survives process exit, TTL-bounded)",
		Long: `Acquire a record-tier hold on the given path. Unlike 'try', the hold
survives process exit and remains authoritative for the duration of its TTL,
which is enforced lazily — there is no daemon. Other agents' 'try' calls
return a holder report until the TTL elapses or 'release <path>' is called.

Default TTL is 5m. Exit codes: 0 success · 1 conflict · 2 usage · 3 wait timeout / system.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			d := defaultAcquireTTL
			if ttl != "" {
				d = parseDurationOrExit("ttl", ttl)
			}
			l := newLOTO()
			tag, conflicts, err := acquireWithWait(l, target, d, wait)
			if err != nil {
				exit(err)
			}
			emitAcquired(target, tag, conflicts)
			return nil
		},
	}
	c.Flags().StringVar(&ttl, "ttl", "", "TTL duration (default 5m)")
	c.Flags().StringVar(&wait, "wait", "", "block until acquired (e.g. 30s, 5m); empty = non-blocking")
	return c
}

// acquireWithWait calls AcquirePath once when wait is empty, otherwise polls
// with exponential backoff until success, a non-ErrHeld error, or the wait
// timer expires. On wait expiry it emits the last-observed holder report to
// stderr and exits with code 3 (distinct from ErrHeld's exit 1) so callers
// can distinguish "told us it could wait but ran out" from "tried and lost".
func acquireWithWait(l *loto.LOTO, target string, ttl time.Duration, wait string) (*loto.Tag, []*loto.Reservation, error) {
	if wait == "" {
		return l.AcquirePath(flagAgent, flagIntent, target, ttl)
	}
	deadline := time.Now().Add(parseDurationOrExit("wait", wait))
	interval := acquirePollStart
	var lastHeld *loto.ErrHeld
	for {
		tag, conflicts, err := l.AcquirePath(flagAgent, flagIntent, target, ttl)
		if err == nil {
			return tag, conflicts, nil
		}
		var held *loto.ErrHeld
		if !errors.As(err, &held) {
			return nil, nil, err
		}
		lastHeld = held
		remaining := time.Until(deadline)
		if remaining <= 0 {
			emitWaitTimeout(lastHeld)
		}
		time.Sleep(min(interval, remaining))
		interval *= 2
		if interval > acquirePollMax {
			interval = acquirePollMax
		}
	}
}

// emitWaitTimeout writes the last-observed holder report to stderr and exits
// with code 3. Mirrors the held-report shape from exit() so callers parsing
// stderr see identical structure to a conflict — only the exit code differs.
func emitWaitTimeout(held *loto.ErrHeld) {
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
		os.Exit(3)
	}
	_ = render.EmitJSON(os.Stderr, held)
	os.Exit(3)
}

func emitAcquired(target string, tag *loto.Tag, conflicts []*loto.Reservation) {
	if currentFormat == render.FormatLLM {
		warnings := make([]render.ReservationWarning, len(conflicts))
		for i, r := range conflicts {
			warnings[i] = render.ReservationWarning{
				Pattern: r.Pattern,
				AgentID: displayAgent(r.AgentID),
			}
		}
		_ = render.EmitLLMAcquired(os.Stdout, render.AcquireEntry{
			Target:    render.RelPath(target),
			AgentID:   displayAgent(tag.AgentID),
			Intent:    tag.Intent,
			ExpiresAt: tag.ExpiresAt,
			Conflicts: warnings,
		})
		return
	}
	result := map[string]any{
		keyAcquired:  true,
		keyTarget:    target,
		keyAgent:     tag.AgentID,
		"expires_at": tag.ExpiresAt,
	}
	if tag.Intent != "" {
		result["intent"] = tag.Intent
	}
	if len(conflicts) > 0 {
		patterns := make([]string, len(conflicts))
		for i, r := range conflicts {
			patterns[i] = r.Pattern + " (" + r.AgentID + ")"
		}
		result["reservation_warnings"] = patterns
	}
	_ = render.EmitJSON(os.Stdout, result)
}
