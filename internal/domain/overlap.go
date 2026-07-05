package domain

import "strings"

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
