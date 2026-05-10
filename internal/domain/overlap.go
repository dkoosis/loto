package domain

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Overlap reports whether two canonical targets could refer to a common path.
// Conservative: false positives tolerated, false negatives are bugs.
func Overlap(a, b Target, caseInsensitive bool) bool {
	ax, bx := a.Canonical, b.Canonical
	if caseInsensitive {
		ax = strings.ToLower(ax)
		bx = strings.ToLower(bx)
	}
	switch {
	case a.Kind == KindGlob || b.Kind == KindGlob:
		return globOverlap(ax, bx)
	case a.Kind == KindDir && b.Kind == KindDir:
		return strings.HasPrefix(ax, bx) || strings.HasPrefix(bx, ax)
	case a.Kind == KindDir:
		return ax == bx+"/" || strings.HasPrefix(bx+"/", ax) || strings.HasPrefix(bx, ax)
	case b.Kind == KindDir:
		return Overlap(b, a, caseInsensitive)
	default:
		return ax == bx
	}
}

func globOverlap(a, b string) bool {
	if a == b {
		return true
	}
	if litA := literalPrefix(a); litA != "" {
		if ok, _ := doublestar.Match(b, litA); ok {
			return true
		}
	}
	if litB := literalPrefix(b); litB != "" {
		if ok, _ := doublestar.Match(a, litB); ok {
			return true
		}
	}
	for _, probe := range []string{a, b, strings.ReplaceAll(a, "**", "x"), strings.ReplaceAll(b, "**", "x")} {
		am, _ := doublestar.Match(a, probe)
		bm, _ := doublestar.Match(b, probe)
		if am && bm {
			return true
		}
	}
	return false
}

func literalPrefix(p string) string {
	idx := strings.IndexAny(p, "*?[{")
	if idx < 0 {
		return p
	}
	cut := strings.LastIndex(p[:idx], "/")
	if cut < 0 {
		return ""
	}
	return p[:cut]
}
