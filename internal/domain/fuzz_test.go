package domain

import (
	"strings"
	"testing"
)

// FuzzCanonicalizeFrom feeds arbitrary tokens, as a caller or git would hand
// them, to the path policy. It must never panic, and a token it accepts must
// be a clean repo-relative path.
func FuzzCanonicalizeFrom(f *testing.F) {
	for _, s := range []string{"a.go", "dir/a.go", "../x", "/abs", "a/./b", "a\x00b", "a\\b", "*.go", "$HOME", " a", "a/", ".", ""} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, in string, git bool) {
		prov := ProvenanceTyped
		if git {
			prov = ProvenanceGit
		}
		got, err := CanonicalizeFrom(in, prov)
		if err != nil {
			return
		}
		c := got.Canonical
		if c == "" || strings.ContainsRune(c, 0) || strings.HasPrefix(c, "/") {
			t.Fatalf("CanonicalizeFrom(%q) = %q: not a clean relative path", in, c)
		}
		for part := range strings.SplitSeq(c, "/") {
			if part == ".." {
				t.Fatalf("CanonicalizeFrom(%q) = %q: escapes the repo", in, c)
			}
		}
	})
}

// FuzzParseHoldRef feeds arbitrary strings to the owner@epoch parser. It must
// never panic, and an accepted ref must round-trip through String.
func FuzzParseHoldRef(f *testing.F) {
	for _, s := range []string{"a@1", "a@b@2", "@1", "a@", "a@-1", "a@99999999999999999999", "", "@", "a@0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := ParseHoldRef(s)
		if err != nil {
			return
		}
		back, err := ParseHoldRef(h.String())
		if err != nil || back != h {
			t.Fatalf("ParseHoldRef(%q) = %v does not round-trip: %v, %v", s, h, back, err)
		}
	})
}
