package domain

// Overlap reports whether two canonical targets refer to the same path.
// Paths are byte-compared; case-insensitive filesystems get OS resolution.
func Overlap(a, b Target) bool {
	return a.Canonical == b.Canonical
}
