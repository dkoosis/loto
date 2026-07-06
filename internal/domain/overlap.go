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
func PrefixOverlaps(a, b string) bool {
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
