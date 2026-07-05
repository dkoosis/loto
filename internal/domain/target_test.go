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
		{tcStorePrefix, tcStorePrefix},
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

func TestCanonicalizePrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{tcStorePrefix, tcStorePrefix},
		{"internal/store/", tcStorePrefix},
		{"internal/store//", tcStorePrefix},
		{"./internal/store", tcStorePrefix},
		{"a//b", "a/b"},
	}
	for _, c := range cases {
		got, err := CanonicalizePrefix(c.in)
		if err != nil {
			t.Fatalf("CanonicalizePrefix(%q) err: %v", c.in, err)
		}
		if got.Canonical != c.want {
			t.Errorf("CanonicalizePrefix(%q) = %+v; want canonical=%q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizePrefixRejections(t *testing.T) {
	cases := []struct {
		in      string
		wantErr error
	}{
		{"", ErrEmptyTarget},
		{"a/*/b", ErrTargetIsGlob},
		{"a\x00b", ErrTargetHasNUL},
		{`a\b`, ErrTargetBackslash},
		{"../escape", ErrRepoEscape},
		{"/abs/path", ErrRepoEscape},
		{".", ErrTargetIsRepoRoot},
		{"./", ErrTargetIsRepoRoot},
	}
	for _, c := range cases {
		_, err := CanonicalizePrefix(c.in)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("CanonicalizePrefix(%q) err=%v; want %v", c.in, err, c.wantErr)
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
