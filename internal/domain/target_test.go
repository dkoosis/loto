package domain

import "testing"

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		in, want string
		kind     TargetKind
	}{
		{"./a", "a", KindFile},
		{"a/./", "a/", KindDir},
		{"a//b", "a/b", KindFile},
		{"internal/store/", "internal/store/", KindDir},
		{"internal/store", "internal/store", KindFile},
		{tcGlobInternal, tcGlobInternal, KindGlob},
		{"./internal/**/foo.go", "internal/**/foo.go", KindGlob},
	}
	for _, c := range cases {
		got, err := Canonicalize(c.in)
		if err != nil {
			t.Fatalf("Canonicalize(%q) err: %v", c.in, err)
		}
		if got.Canonical != c.want || got.Kind != c.kind {
			t.Errorf("Canonicalize(%q) = %+v; want canonical=%q kind=%v", c.in, got, c.want, c.kind)
		}
	}
}

func TestCanonicalizeRejectsRepoEscape(t *testing.T) {
	if _, err := Canonicalize("../../etc/passwd"); err == nil {
		t.Fatal("expected error for repo-escape target")
	}
}

func TestCanonicalizeRejectsAbsolutePath(t *testing.T) {
	for _, in := range []string{"/tmp/x", "/etc/passwd"} {
		if _, err := Canonicalize(in); err == nil {
			t.Errorf("expected error for absolute path %q", in)
		}
	}
}

func TestCanonicalizeRejectsBackslashPath(t *testing.T) {
	if _, err := Canonicalize(`a\b.go`); err == nil {
		t.Fatal("expected error for backslash in target (storage is POSIX-style)")
	}
}

func TestCanonicalizeRejectsNULTarget(t *testing.T) {
	if _, err := Canonicalize("a\x00b"); err == nil {
		t.Fatal("expected error for NUL byte in target")
	}
}
