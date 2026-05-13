package domain

import (
	"errors"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"./a", "a"},
		{"a//b", "a/b"},
		{"internal/store", "internal/store"},
	}
	for _, c := range cases {
		got, err := Canonicalize(c.in)
		if err != nil {
			t.Fatalf("Canonicalize(%q) err: %v", c.in, err)
		}
		if got.Canonical != c.want {
			t.Errorf("Canonicalize(%q) = %+v; want canonical=%q", c.in, got, c.want)
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

func TestCanonicalizeRejectsGlob(t *testing.T) {
	for _, in := range []string{"*.go", "a/b/*.go", "a/?.go", "a/[abc].go", "a/{x,y}.go"} {
		_, err := Canonicalize(in)
		if !errors.Is(err, ErrTargetIsGlob) {
			t.Errorf("Canonicalize(%q) err=%v; want ErrTargetIsGlob", in, err)
		}
	}
}

func TestCanonicalizeRejectsTrailingSlash(t *testing.T) {
	for _, in := range []string{"foo/", "a/b/"} {
		_, err := Canonicalize(in)
		if !errors.Is(err, ErrTargetIsDir) {
			t.Errorf("Canonicalize(%q) err=%v; want ErrTargetIsDir", in, err)
		}
	}
}
