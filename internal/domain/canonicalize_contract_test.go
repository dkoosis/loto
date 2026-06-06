package domain_test

import (
	"path"
	"strings"
	"testing"

	cmp "loto/internal/testcmp"
	require "loto/internal/testrequire"

	"loto/internal/domain"
)

func TestCanonicalize_ReturnsStableRepoRelativeTarget_WhenInputIsExplicitFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    domain.Target
		wantErr error
		inspect func(*testing.T, domain.Target)
	}{
		{
			name:    "rejects empty target",
			input:   "",
			wantErr: domain.ErrEmptyTarget,
		},
		{
			name:    "rejects absolute path escaping repo",
			input:   "/etc/passwd",
			wantErr: domain.ErrRepoEscape,
		},
		{
			name:    "rejects parent traversal escaping repo",
			input:   "../../etc/passwd",
			wantErr: domain.ErrRepoEscape,
		},
		{
			name:    "rejects malformed path containing nul",
			input:   "a\x00b.go",
			wantErr: domain.ErrTargetHasNUL,
		},
		{
			name:    "rejects glob instead of explicit path",
			input:   "internal/domain/*.go",
			wantErr: domain.ErrTargetIsGlob,
		},
		{
			name:    "rejects repo root boundary",
			input:   ".",
			wantErr: domain.ErrTargetIsRepoRoot,
		},
		{
			name:    "rejects trailing slash directory boundary",
			input:   "internal/domain/",
			wantErr: domain.ErrTargetIsDir,
		},
		{
			name:  "cleans redundant current directory segments",
			input: "./internal/domain/./target.go",
			want:  domain.Target{Canonical: "internal/domain/target.go"},
			inspect: func(t *testing.T, got domain.Target) {
				t.Helper()
				assertCanonicalTargetInvariant(t, got)
			},
		},
		{
			name:  "cleans inner parent segments without escaping repo",
			input: "internal/domain/../store/store.go",
			want:  domain.Target{Canonical: "internal/store/store.go"},
			inspect: func(t *testing.T, got domain.Target) {
				t.Helper()
				assertCanonicalTargetInvariant(t, got)
			},
		},
		{
			name:  "preserves already canonical explicit file path",
			input: "cmd/loto/main.go",
			want:  domain.Target{Canonical: "cmd/loto/main.go"},
			inspect: func(t *testing.T, got domain.Target) {
				t.Helper()
				assertCanonicalTargetInvariant(t, got)
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.Canonicalize(tc.input)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("diff (-want +got):\n%s", diff)
			}

			if tc.inspect != nil {
				tc.inspect(t, got)
			}
		})
	}
}

func assertCanonicalTargetInvariant(t *testing.T, got domain.Target) {
	t.Helper()

	require.NotEmpty(t, got.Canonical)
	require.False(t, strings.HasPrefix(got.Canonical, "/"), "canonical target must remain repository-relative")
	require.False(t, strings.HasPrefix(got.Canonical, "../"), "canonical target must not escape the repository")
	require.NotEqual(t, "..", got.Canonical, "canonical target must not escape the repository")
	require.NotEqual(t, ".", got.Canonical, "canonical target must not be the repository root")
	require.False(t, strings.HasSuffix(got.Canonical, "/"), "canonical target must be an explicit non-directory path")
	require.NotContains(t, got.Canonical, "\\", "canonical target must use POSIX separators")
	require.NotContains(t, got.Canonical, "\x00", "canonical target must not contain NUL")
	require.False(t, strings.ContainsAny(got.Canonical, "*?[{"), "canonical target must not contain glob syntax")
	require.Equal(t, path.Clean(got.Canonical), got.Canonical, "canonical target must be path-clean")

	recanonicalized, err := domain.Canonicalize(got.Canonical)
	require.NoError(t, err)
	require.True(t, domain.SameCanonical(got, recanonicalized), "canonicalization must be idempotent")
}
