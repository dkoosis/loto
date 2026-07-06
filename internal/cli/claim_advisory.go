package cli

import (
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
)

// foreignClaimAdvisory is one (target, covering foreign live claim) pair —
// the advisory counterpart of render.GateDenyRow's claim kind, but never a
// deny: lock/check surface it as a ⚠ row while the operation itself
// proceeds unchanged (loto-qoq, cooperative model).
type foreignClaimAdvisory struct {
	Target string // canonical
	Owner  string
	Intent string
	Prefix string
}

// foreignClaimAdvisoriesFor is the shared ListClaims → collect step behind both
// loto check and loto lock's ⚠ advisory. ListClaims errors are swallowed
// silently: the advisory is best-effort and must never mask the primary
// lock/check result (same posture as fetchTagsForBlockers). loto-qoq.
func foreignClaimAdvisoriesFor(rt *runtime, targets []domain.Target, now time.Time) []foreignClaimAdvisory {
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		return nil
	}
	return collectForeignClaimAdvisories(targets, claims, rt.Agent.UUID, now)
}

// collectForeignClaimAdvisories returns one advisory per (target, covering
// live foreign claim), deduped and deterministically sorted by
// (Target, Owner, Prefix). Empty when no claim covers any target. Shares
// domain.ClaimCoversTarget with check --gate's gateDecide — same predicate,
// two dispositions (deny vs advisory).
func collectForeignClaimAdvisories(targets []domain.Target, claims []domain.ClaimRecord, myUUID string, now time.Time) []foreignClaimAdvisory {
	var rows []foreignClaimAdvisory
	seen := map[string]bool{}
	for _, t := range targets {
		for i := range claims {
			c := &claims[i]
			if !domain.ClaimCoversTarget(*c, t.Canonical, myUUID, now) {
				continue
			}
			key := t.Canonical + "|" + c.PathPrefix + "|" + string(c.OwnerUUID)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, foreignClaimAdvisory{
				Target: t.Canonical,
				Owner:  string(c.OwnerUUID),
				Intent: c.Intent,
				Prefix: c.PathPrefix,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Target != rows[j].Target {
			return rows[i].Target < rows[j].Target
		}
		if rows[i].Owner != rows[j].Owner {
			return rows[i].Owner < rows[j].Owner
		}
		return rows[i].Prefix < rows[j].Prefix
	})
	return rows
}

// emitForeignClaimAdvisories prints the ⚠ under-claim block: a count header
// (parity with every other sibling block — ✓ locked count=, ✗ blocked
// count=, ✓ claims count=) then one row per advisory. Emits nothing when
// rows is empty — silence is correct here, since the advisory is secondary
// to the primary lock/check result already printed above it.
func emitForeignClaimAdvisories(w io.Writer, rows []foreignClaimAdvisory) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "⚠ under-claim count=%d\n", len(rows))
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(w, "⚠ target=%s under-claim owner=%s intent=%q prefix=%s\n",
			relPath(r.Target), r.Owner, r.Intent, relPath(r.Prefix))
	}
}
