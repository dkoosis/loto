package cli

import (
	"io"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

// territoryTagCovers is the "a note is pinned to ground I am touching"
// predicate: live, unacked, and its prefix overlaps the target.
//
// ‡ domain.PrefixOverlaps is shared with claims (loto-7af9) rather than
// reimplemented. That is what makes `loto claim internal/store` and
// `loto tag internal/store "…"` cover identically the same ground, including
// the segment-boundary rule that keeps `internal/store` off
// `internal/storefront`. A second prefix definition would be a second thing to
// keep true.
func territoryTagCovers(n store.TerritoryTag, targetCanonical string, nowNs int64) bool {
	return n.Live(nowNs) && domain.PrefixOverlaps(n.PathPrefix, targetCanonical)
}

// liveTerritoryTags loads every note and keeps the live ones. A read-all is the
// right shape here for the same reason it is for claims: the table is capped at
// 50 live rows, so there is no prefix SQL to write and none to get wrong.
//
// A store error answers nil rather than propagating. Every caller is a footer
// or an advisory block hung off a command whose real work already succeeded —
// failing a completed `loto lock` because a note could not be read would be
// the tail wagging the dog.
func liveTerritoryTags(rt *runtime, now time.Time) []store.TerritoryTag {
	notes, err := rt.Store.ListTerritoryTags(rt.Ctx)
	if err != nil {
		return nil
	}
	nowNs := now.UnixNano()
	out := notes[:0:0]
	for _, n := range notes {
		if n.Live(nowNs) {
			out = append(out, n)
		}
	}
	return out
}

// territoryTagsForTargets keeps the notes covering any of the named targets,
// deduped by id. Used by the surfaces that ask about paths the caller supplied
// — plain `check` — rather than about ground the caller holds.
func territoryTagsForTargets(notes []store.TerritoryTag, targets []domain.Target, now time.Time) []store.TerritoryTag {
	nowNs := now.UnixNano()
	seen := map[string]bool{}
	var out []store.TerritoryTag
	for _, n := range notes {
		for i := range targets {
			if seen[n.ID] {
				break
			}
			if territoryTagCovers(n, targets[i].Canonical, nowNs) {
				seen[n.ID] = true
				out = append(out, n)
			}
		}
	}
	sortTerritoryTags(out)
	return out
}

// territoryTagsForHeldGround keeps the notes covering territory this agent now
// holds — a locked file or a claimed prefix — and drops the caller's own.
//
// ‡ The self-exclusion is not a new rule. ListAliveForOwner already filters
// `tagger_uuid <> owner` on the holder-facing surface while ListAliveForTarget
// deliberately does not on the others (tags.go). Territory tags inherit both
// halves, so the codebase keeps one convention rather than growing a second:
// the footer answers "what should I know about ground I just took", and being
// told your own note is noise; `check` and `status` answer "what is pinned
// here", where your own note is part of the answer.
func territoryTagsForHeldGround(rt *runtime, notes []store.TerritoryTag, now time.Time) []store.TerritoryTag {
	if len(notes) == 0 {
		return nil
	}
	ground := heldGround(rt)
	if len(ground) == 0 {
		return nil
	}
	nowNs := now.UnixNano()
	var out []store.TerritoryTag
	for _, n := range notes {
		if n.TaggerUUID == rt.Agent.UUID {
			continue
		}
		for _, g := range ground {
			if territoryTagCovers(n, g, nowNs) {
				out = append(out, n)
				break
			}
		}
	}
	sortTerritoryTags(out)
	return out
}

// heldGround is every canonical path this agent currently holds a lock on plus
// every prefix it has claimed — the ground a note has to overlap to be worth
// telling the caller about.
func heldGround(rt *runtime) []string {
	var out []string
	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err == nil {
		for i := range locks {
			if string(locks[i].OwnerUUID) == rt.Agent.UUID {
				out = append(out, locks[i].Target.Canonical)
			}
		}
	}
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err == nil {
		for i := range claims {
			if string(claims[i].OwnerUUID) == rt.Agent.UUID {
				out = append(out, claims[i].PathPrefix)
			}
		}
	}
	return out
}

// sortTerritoryTags fixes the render order: prefix, then created_at, then id.
// Same input, byte-identical output (.claude/rules/design.md).
func sortTerritoryTags(notes []store.TerritoryTag) {
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].PathPrefix != notes[j].PathPrefix {
			return notes[i].PathPrefix < notes[j].PathPrefix
		}
		if notes[i].CreatedAt != notes[j].CreatedAt {
			return notes[i].CreatedAt < notes[j].CreatedAt
		}
		return notes[i].ID < notes[j].ID
	})
}

// emitTerritoryTagFooter is the DeferredTagFooter's territory half: notes
// pinned to ground the caller now holds, printed after the command's own rows.
func emitTerritoryTagFooter(w io.Writer, notes []store.TerritoryTag) {
	render.EmitTerritoryTagRows(w, notes, "territory-tags", "")
}
