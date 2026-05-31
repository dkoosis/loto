package domain

import "testing"

func TestSameCanonical(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		name string
	}{
		{"a", "a", true, "exact-eq"},
		{"a", "b", false, "exact-diff"},
		{tcAxGo, tcAxGo, true, "nested-eq"},
		{tcAxGo, "b/x.go", false, "literal-disjoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ta, _ := Canonicalize(c.a)
			tb, _ := Canonicalize(c.b)
			if got := SameCanonical(ta, tb); got != c.want {
				t.Errorf("SameCanonical(%q,%q) = %v; want %v", c.a, c.b, got, c.want)
			}
			if got := SameCanonical(tb, ta); got != c.want {
				t.Errorf("SameCanonical(%q,%q) symmetry = %v; want %v", c.b, c.a, got, c.want)
			}
		})
	}
}
