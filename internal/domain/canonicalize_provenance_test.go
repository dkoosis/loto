package domain

import (
	"errors"
	"testing"
)

// Provenance moves exactly one rule, and this file pins both directions of it
// (loto-pgio). The shell-token rule refuses an unexpanded token a CALLER typed
// (loto-bl66); a path git printed never passed through a shell, so the same
// spelling is an ordinary filename there.

func TestCanonicalizeFrom_GitProvenanceAdmitsShellSpellings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "double quote in a filename", input: `say "hi".go`, want: `say "hi".go`},
		{name: "dollar sign in a filename", input: "cost$.go", want: "cost$.go"},
		{name: "backtick in a filename", input: "we`ird.go", want: "we`ird.go"},
		{name: "single quote in a filename", input: "it's.go", want: "it's.go"},
		{name: "tab in a filename", input: "a\tb.go", want: "a\tb.go"},
		{name: "newline in a filename", input: "a\nb.go", want: "a\nb.go"},
		{name: "trailing space in a filename", input: "trail.go ", want: "trail.go "},
		{name: "leading space in a filename", input: " lead.go", want: " lead.go"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := CanonicalizeFrom(tc.input, ProvenanceTyped); !errors.Is(err, ErrTargetUnspellable) {
				t.Errorf("typed %q err = %v; want %v", tc.input, err, ErrTargetUnspellable)
			}
			got, err := CanonicalizeFrom(tc.input, ProvenanceGit)
			if err != nil {
				t.Fatalf("git %q unexpected err: %v", tc.input, err)
			}
			if got.Canonical != tc.want {
				t.Errorf("git %q = %q; want %q", tc.input, got.Canonical, tc.want)
			}
		})
	}
}

// Every rule except the shell-token one still binds under git provenance —
// containment above all. A path git printed is not a licence to leave the
// repo, and a `..` in an index nobody vetted would otherwise become a lock key
// pointing anywhere on disk.
func TestCanonicalizeFrom_GitProvenanceKeepsEveryOtherRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "still refuses empty", input: "", wantErr: ErrEmptyTarget},
		{name: "still refuses NUL", input: "a\x00b.go", wantErr: ErrTargetHasNUL},
		{name: "still refuses backslash", input: `a\b.go`, wantErr: ErrTargetBackslash},
		{name: "still refuses an absolute path", input: "/tmp/outside.go", wantErr: ErrRepoEscape},
		{name: "still refuses parent traversal", input: "../../etc/passwd", wantErr: ErrRepoEscape},
		{name: "still refuses a glob", input: "internal/*.go", wantErr: ErrTargetIsGlob},
		{name: "still refuses a directory spelling", input: "internal/domain/", wantErr: ErrTargetIsDir},
		{name: "still refuses the repo root", input: ".", wantErr: ErrTargetIsRepoRoot},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := CanonicalizeFrom(tc.input, ProvenanceGit); !errors.Is(err, tc.wantErr) {
				t.Errorf("git %q err = %v; want %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

// Canonicalize is the typed provenance, unchanged: every caller that did not
// ask for the carve-out keeps the strict policy.
func TestCanonicalize_IsTypedProvenance(t *testing.T) {
	t.Parallel()

	if _, err := Canonicalize(`say "hi".go`); !errors.Is(err, ErrTargetUnspellable) {
		t.Errorf("Canonicalize must stay strict: err = %v", err)
	}
	if _, err := CanonicalizePrefix("$FAKE_HOME/"); !errors.Is(err, ErrTargetUnspellable) {
		t.Errorf("CanonicalizePrefix must stay strict: err = %v", err)
	}
}
