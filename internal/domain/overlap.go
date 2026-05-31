package domain

// SameCanonical reports whether two targets share the same canonical path.
// Paths are byte-compared; case-insensitive filesystems get OS resolution.
func SameCanonical(a, b Target) bool {
	return a.Canonical == b.Canonical
}
