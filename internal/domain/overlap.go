package domain

import (
	"strings"
	"time"
)

// SameCanonical reports whether two targets share the same canonical path.
// Paths are byte-compared; case-insensitive filesystems get OS resolution.
func SameCanonical(a, b Target) bool {
	return a.Canonical == b.Canonical
}

// PrefixOverlaps reports whether two canonical path prefixes reserve
// overlapping territory: equal, or one a path-segment ancestor of the other.
// The "/"-suffixed comparison keeps the boundary on segments, so
// internal/store does NOT overlap internal/storefront (loto-7af9).
//
// sd-isv2: "." is the repo root, the widest prefix, so it overlaps every other
// prefix and itself. This arm is not a convenience — NONE of the three below
// fires for it. A canonical path never starts "./", so HasPrefix(b, "./") is
// false for every b, and HasPrefix(".", b+"/") is false for every b too.
// Without it CanonicalizePrefix would accept a root claim that then covered
// nothing: quieter than the refusal it replaced and strictly worse, because a
// takeover would report itself reserved while blocking no peer at all.
func PrefixOverlaps(a, b string) bool {
	if a == "." || b == "." {
		return true
	}
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// ClaimCoversTarget reports whether claim c is a live, foreign claim whose
// prefix covers target canonical t at now — the "you're inside someone
// else's reserved territory" predicate shared by check --gate (deny) and
// lock/check (advisory).
func ClaimCoversTarget(c ClaimRecord, t string, myUUID string, now time.Time) bool {
	return string(c.OwnerUUID) != myUUID &&
		!c.Expired(now) &&
		PrefixOverlaps(c.PathPrefix, t)
}

// SameTarget, PrefixCovers and ClaimCovers are the three predicates above
// widened by EvalContext.CaseFold (loto-8soe). They live here, beside the
// byte-exact primitives they delegate to, so a reader comparing two keys sees
// both halves of the rule in one place.
//
// ‡ Every conflict decision goes through one of these three. A key minted by
// this binary on a case-folding filesystem is already folded, so on a fresh
// store the widened arm never fires; it exists for the row a PREVIOUS loto
// wrote in the on-disk spelling (`Makefile`, `docs/README.md`) and that is
// still live when the folded key (`makefile`) arrives. Without it that row
// stops blocking the moment this binary lands, which is the double-grant the
// fold set out to close, arriving through the upgrade instead.
//
// ‡ The widened arm is fail-SAFE in the one direction that matters: it can
// only find MORE conflicts, never fewer. c.CaseFold is false on a
// case-sensitive filesystem and in every zero-value EvalContext, where a.go
// and A.go are two files and must stay independently lockable.

// SameTarget reports whether a and b name the same file under this context's
// filesystem case class.
func (c EvalContext) SameTarget(a, b Target) bool {
	if SameCanonical(a, b) {
		return true
	}
	return c.CaseFold && strings.EqualFold(a.Canonical, b.Canonical)
}

// PrefixCovers is PrefixOverlaps under this context's filesystem case class.
func (c EvalContext) PrefixCovers(a, b string) bool {
	if PrefixOverlaps(a, b) {
		return true
	}
	return c.CaseFold && PrefixOverlaps(strings.ToLower(a), strings.ToLower(b))
}

// ClaimCovers is ClaimCoversTarget under this context's filesystem case class,
// reading the clock from the context rather than a positional now.
func (c EvalContext) ClaimCovers(cl ClaimRecord, t string, myUUID string) bool {
	return !cl.Expired(c.Now) && c.ClaimTerritoryCovers(cl, t, myUUID)
}

// ClaimTerritoryCovers is ClaimCovers with the TTL filter removed: foreign
// owner, and a prefix that reserves t under this context's case class. It
// answers "whose territory is this path in?" without also answering "is that
// reservation still in force?".
//
// ‡ Every gate wants both questions and should keep calling ClaimCovers. The
// one caller that needs them apart is `loto sync` (loto-3dhl), which must look
// at a LAPSED claim before it overwrites bytes inside it — the expiry filter
// made a lapsed claim invisible, so a live agent's uncommitted edits
// fast-forwarded away while the agent was still writing them. Splitting the
// predicate makes that question answerable without widening what any existing
// caller sees.
func (c EvalContext) ClaimTerritoryCovers(cl ClaimRecord, t string, myUUID string) bool {
	return string(cl.OwnerUUID) != myUUID &&
		c.PrefixCovers(cl.PathPrefix, t)
}
