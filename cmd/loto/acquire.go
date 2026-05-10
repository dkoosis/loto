package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"loto"
	"loto/internal/render"
)

const defaultAcquireTTL = 5 * time.Minute

// acquireCmd registers `loto acquire <path>` — the record-tier blocking
// hold introduced by loto-ux3.1. Default TTL is 5m. --wait is reserved
// for a follow-up step (Step 12); current form fails immediately on
// conflict per Step 10's happy-path scope.
func acquireCmd() *cobra.Command {
	var ttl string
	c := &cobra.Command{
		Use:   "acquire <path>",
		Short: "record-tier hold on a path (survives process exit, TTL-bounded)",
		Long: `Acquire a record-tier hold on the given path. Unlike 'try', the hold
survives process exit and remains authoritative for the duration of its TTL,
which is enforced lazily — there is no daemon. Other agents' 'try' calls
return a holder report until the TTL elapses or 'release <path>' is called.

Default TTL is 5m. Exit codes: 0 success · 1 conflict · 2 usage · 3 system.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			d := defaultAcquireTTL
			if ttl != "" {
				d = parseDurationOrExit("ttl", ttl)
			}
			l := newLOTO()
			tag, conflicts, err := l.AcquirePath(flagAgent, flagIntent, target, d)
			if err != nil {
				exit(err)
			}
			emitAcquired(target, tag, conflicts)
			return nil
		},
	}
	c.Flags().StringVar(&ttl, "ttl", "", "TTL duration (default 5m)")
	return c
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
		"acquired":   true,
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
