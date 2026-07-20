package cli

import (
	"fmt"
	"time"

	"loto/internal/domain"
)

// notifyReleasedWaiters mails each waiter whose tag this agent's release just
// acked, closing the blocked-waiter loop (loto-4lc): the waiter tagged a blocked
// target and moved on, and this mail surfaces on their next command's footer to
// pull them back — no daemon, no polling, no idle sitting.
//
// sinceNs is a timestamp captured BEFORE the release call; TaggersAckedSince then
// reads the tags the release acked at/after it. Reading by ack-time after the
// release (rather than tag rows before it) closes the race a read-before would
// open — a tag committed in the gap is acked by the release and still caught
// (Codex #223 P1). Best-effort: mail is a channel, not an invariant, so every
// error renders as silence and never fails the unlock.
func (r *runtime) notifyReleasedWaiters(sinceNs int64) {
	waiters, err := r.Store.TaggersAckedSince(r.Ctx, domain.AgentUUID(r.Agent.UUID), sinceNs)
	if err != nil || len(waiters) == 0 {
		return
	}
	box, err := r.OpenMail()
	if err != nil {
		return
	}
	defer box.Close()
	from := r.Agent.Handle
	if from == "" {
		from = shortID(r.Agent.UUID)
	}
	for i := range waiters {
		body := fmt.Sprintf("loto: %s released by %s — retry your lock", relPath(waiters[i].TargetCanonical), from)
		_, _ = box.Send(r.Ctx, domain.Message{
			FromUUID:   r.agentUUID(),
			FromHandle: r.Agent.Handle,
			To:         waiters[i].TaggerUUID,
			Body:       body,
		})
	}
}

// nowNanos is the pre-release timestamp handed to notifyReleasedWaiters. Split
// out so the two release call sites read identically.
func nowNanos() int64 { return time.Now().UnixNano() }

// shortID trims a UUID to its first 8 chars for a compact sender label, matching
// the inbox/summary fallback when no handle is set.
func shortID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}
