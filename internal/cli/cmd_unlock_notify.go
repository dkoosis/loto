package cli

import (
	"fmt"

	"loto/internal/domain"
	"loto/internal/store"
)

// collectWaiterTags returns the live tags sitting on this agent's locks — the
// agents-in-waiting who hit a blocked `loto check` and left a `loto tag`
// breadcrumb instead of sitting on the lock. MUST be read BEFORE the release:
// release acks these tags, so a post-release read finds nothing (loto-4lc).
//
// An empty targets slice reads every tag on the holder's locks — the
// `unlock --all` / session-end path; a non-empty slice scopes to those targets.
func (r *runtime) collectWaiterTags(targets []domain.Target) []store.Tag {
	holder := r.Agent.UUID
	if len(targets) == 0 {
		tags, err := r.Store.ListAliveForOwner(r.Ctx, domain.AgentUUID(holder))
		if err != nil {
			return nil
		}
		return tags
	}
	cans := make([]domain.Canonical, len(targets))
	for i := range targets {
		cans[i] = domain.Canonical(targets[i].Canonical)
	}
	byTarget, err := r.Store.ListAliveByTargets(r.Ctx, cans)
	if err != nil {
		return nil
	}
	var out []store.Tag
	for _, ts := range byTarget {
		for i := range ts {
			if ts[i].LockOwnerUUID == holder {
				out = append(out, ts[i])
			}
		}
	}
	return out
}

// notifyWaiters mails each distinct tagger whose breadcrumb sat on a target this
// release actually freed, closing the blocked-waiter loop (loto-4lc): the waiter
// tagged and moved on, and this mail surfaces on their next command's footer to
// pull them back — no daemon, no polling, no sitting idle. Best-effort: mail is
// a channel, not an invariant, so every error renders as silence (never fails
// the unlock). A tagger is mailed at most once per freed path; self-tags (a
// holder tagging their own lock) are skipped.
func (r *runtime) notifyWaiters(freed map[string]bool, tags []store.Tag) {
	type key struct{ target, tagger string }
	seen := map[key]bool{}
	var toSend []store.Tag
	for i := range tags {
		tg := &tags[i]
		if !freed[string(tg.TargetCanonical)] || tg.TaggerUUID == r.Agent.UUID {
			continue
		}
		k := key{string(tg.TargetCanonical), tg.TaggerUUID}
		if seen[k] {
			continue
		}
		seen[k] = true
		toSend = append(toSend, *tg)
	}
	if len(toSend) == 0 {
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
	for i := range toSend {
		tg := &toSend[i]
		body := fmt.Sprintf("loto: %s released by %s — retry your lock", relPath(string(tg.TargetCanonical)), from)
		_, _ = box.Send(r.Ctx, domain.Message{
			FromUUID:   r.agentUUID(),
			FromHandle: r.Agent.Handle,
			To:         tg.TaggerUUID,
			Body:       body,
		})
	}
}

// freedFromReleases collects the canonical paths a batch release actually freed
// (unlocked outright, or reclaimed a stale foreign lock) — the targets whose
// waiters should be notified.
func freedFromReleases(results []store.ReleaseResult) map[string]bool {
	m := make(map[string]bool, len(results))
	for i := range results {
		if results[i].State == store.StateUnlocked || results[i].State == store.StateReclaimedStale {
			m[results[i].Target.Canonical] = true
		}
	}
	return m
}

// freedFromBreaks collects the canonical paths a --force break actually removed
// (Err == nil). A restore-failure still freed the lock row, so RestoreErr does
// not disqualify a target from notifying its waiters.
func freedFromBreaks(results []store.BreakResult) map[string]bool {
	m := make(map[string]bool, len(results))
	for i := range results {
		if results[i].Err == nil {
			m[results[i].Target.Canonical] = true
		}
	}
	return m
}

// shortID trims a UUID to its first 8 chars for a compact sender label, matching
// the inbox/summary fallback when no handle is set.
func shortID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}
