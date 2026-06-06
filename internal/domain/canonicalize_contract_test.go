package domain

import (
	"errors"
	"path"
	"strings"
	"testing"
)

// TestCanonicalize_Contract is a contract safety net for Canonicalize.
// It documents and enforces the public contract: which inputs are rejected
// (with which sentinel error), and which invariants every accepted Target must
// satisfy. Plain stdlib assertions, matching the package's test convention.
func TestCanonicalize_Contract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string // expected Canonical when wantErr is nil
		wantErr error
	}{
		{name: "rejects empty target", input: "", wantErr: ErrEmptyTarget},
		{name: "rejects absolute path escaping repo", input: "/etc/passwd", wantErr: ErrRepoEscape},
		{name: "rejects parent traversal escaping repo", input: "../../etc/passwd", wantErr: ErrRepoEscape},
		{name: "rejects malformed path containing nul", input: "a\x00b.go", wantErr: ErrTargetHasNUL},
		{name: "rejects backslash separator", input: `a\b.go`, wantErr: ErrTargetBackslash},
		{name: "rejects glob instead of explicit path", input: "internal/domain/*.go", wantErr: ErrTargetIsGlob},
		{name: "rejects repo root boundary", input: ".", wantErr: ErrTargetIsRepoRoot},
		{name: "rejects trailing slash directory boundary", input: "internal/domain/", wantErr: ErrTargetIsDir},
		{name: "cleans redundant current directory segments", input: "./internal/domain/./target.go", want: "internal/domain/target.go"},
		{name: "cleans inner parent segments without escaping repo", input: "internal/domain/../store/store.go", want: "internal/store/store.go"},
		{name: "preserves already canonical explicit file path", input: "cmd/loto/main.go", want: "cmd/loto/main.go"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Canonicalize(tc.input)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Canonicalize(%q) err = %v; want %v", tc.input, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Canonicalize(%q) unexpected err: %v", tc.input, err)
			}
			if got.Canonical != tc.want {
				t.Errorf("Canonicalize(%q) = %q; want %q", tc.input, got.Canonical, tc.want)
			}
			assertCanonicalTargetInvariant(t, got)
		})
	}
}

// assertCanonicalTargetInvariant asserts the properties every accepted Target
// must hold, regardless of input: repo-relative, non-empty, not the root, no
// NUL/backslash/glob syntax, path-clean, and idempotent under re-canonicalization.
func assertCanonicalTargetInvariant(t *testing.T, got Target) {
	t.Helper()

	c := got.Canonical
	if c == "" {
		t.Error("canonical target must be non-empty")
	}
	if strings.HasPrefix(c, "/") || strings.HasPrefix(c, "../") || c == ".." {
		t.Errorf("canonical target %q must remain repository-relative", c)
	}
	if c == "." {
		t.Errorf("canonical target %q must not be the repository root", c)
	}
	if strings.HasSuffix(c, "/") {
		t.Errorf("canonical target %q must be an explicit non-directory path", c)
	}
	if strings.ContainsAny(c, "\\\x00") {
		t.Errorf("canonical target %q must not contain backslash or NUL", c)
	}
	if strings.ContainsAny(c, "*?[{") {
		t.Errorf("canonical target %q must not contain glob syntax", c)
	}
	if cleaned := path.Clean(c); cleaned != c {
		t.Errorf("canonical target %q must be path-clean (got %q)", c, cleaned)
	}

	recanonicalized, err := Canonicalize(c)
	if err != nil {
		t.Fatalf("re-canonicalizing %q failed: %v", c, err)
	}
	if !SameCanonical(got, recanonicalized) {
		t.Errorf("canonicalization must be idempotent: %q -> %q", c, recanonicalized.Canonical)
	}
}
