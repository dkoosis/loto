package domain

import "testing"

func TestOverlap(t *testing.T) {
	caseInsensitive := false
	cases := []struct {
		a, b string
		want bool
		name string
	}{
		{"a", "a", true, "exact-eq"},
		{"a", "b", false, "exact-diff"},
		{"a/", "a", true, "dir-vs-exact-base"},
		{"a/", "a/b/c.go", true, "dir-vs-nested"},
		{"a/", "b/c.go", false, "dir-vs-disjoint"},
		{"a/", "a/", true, "dir-eq"},
		{"a/", "a/b/", true, "dir-vs-subdir"},
		{tcGlobInternal, "internal/store/x.go", true, "glob-vs-literal"},
		{tcGlobInternal, "cmd/loto/x.go", false, "glob-vs-disjoint"},
		{"**", "anything/here.go", true, "global-vs-anything"},
		{tcGlobADoubleStar, tcGlobADoubleStar, true, "glob-eq"},
		{tcGlobADoubleStar, "a/b/**", true, "glob-prefix-contain"},
		{"a/x.go", "b/x.go", false, "literal-disjoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ta, _ := Canonicalize(c.a)
			tb, _ := Canonicalize(c.b)
			if got := Overlap(ta, tb, caseInsensitive); got != c.want {
				t.Errorf("Overlap(%q,%q,ci=%v) = %v; want %v", c.a, c.b, caseInsensitive, got, c.want)
			}
			if got := Overlap(tb, ta, caseInsensitive); got != c.want {
				t.Errorf("Overlap(%q,%q,ci=%v) symmetry = %v; want %v", c.b, c.a, caseInsensitive, got, c.want)
			}
		})
	}
}

func TestOverlapCaseInsensitive(t *testing.T) {
	a, _ := Canonicalize("Foo.go")
	b, _ := Canonicalize("foo.go")
	if !Overlap(a, b, true) {
		t.Error("case-insensitive: Foo.go and foo.go must overlap")
	}
	if Overlap(a, b, false) {
		t.Error("case-sensitive: Foo.go and foo.go must NOT overlap")
	}
}
