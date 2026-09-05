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
