package domain_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"loto/internal/domain"
)

type requireFns struct{}

var require requireFns

func (requireFns) ErrorIs(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v, got %v", target, err)
	}
}
func (requireFns) NoError(t *testing.T, err error) { t.Helper(); if err != nil { t.Fatalf("expected no error, got %v", err) } }
func (requireFns) NotEmpty(t *testing.T, s string) { t.Helper(); if s == "" { t.Fatal("expected non-empty string") } }
func (requireFns) NotContains(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) { t.Fatalf("expected %q not to contain %q", s, sub) }
}
func (requireFns) NotEqualString(t *testing.T, a, b string) { t.Helper(); if a == b { t.Fatalf("expected %q != %q", a, b) } }
func (requireFns) NotEqualByte(t *testing.T, a, b byte)     { t.Helper(); if a == b { t.Fatalf("expected %v != %v", a, b) } }

var cmp = struct{ Diff func(want, got any) string }{Diff: func(want, got any) string {
	if reflect.DeepEqual(want, got) { return "" }
	return fmt.Sprintf("want=%#v got=%#v", want, got)
}}

func TestCanonicalize_ExpectedBehaviour_When_NormalizingAndValidatingTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    domain.Target
		wantErr error
		inspect func(*testing.T, domain.Target)
	}{
		{name: "error on empty target", input: "", wantErr: domain.ErrEmptyTarget},
		{name: "error on absolute path repo escape", input: "/etc/passwd", wantErr: domain.ErrRepoEscape},
		{name: "error on glob target", input: "a/*.go", wantErr: domain.ErrTargetIsGlob},
		{name: "boundary rejects repo root dot", input: ".", wantErr: domain.ErrTargetIsRepoRoot},
		{name: "happy path canonicalizes current directory prefix and duplicate separators", input: "./a//b.go", want: domain.Target{Canonical: "a/b.go"}, inspect: func(t *testing.T, got domain.Target) {
			t.Helper(); require.NotEmpty(t, got.Canonical); require.NotEqualString(t, ".", got.Canonical); require.NotContains(t, got.Canonical, "//"); require.NotEqualByte(t, byte('/'), got.Canonical[0])
		}},
		{name: "happy path keeps already canonical relative file", input: "internal/domain/target.go", want: domain.Target{Canonical: "internal/domain/target.go"}, inspect: func(t *testing.T, got domain.Target) {
			t.Helper(); require.NotContains(t, got.Canonical, `\`); require.NotContains(t, got.Canonical, "../")
		}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.Canonicalize(tc.input)
			if tc.wantErr != nil { require.ErrorIs(t, err, tc.wantErr); return }
			require.NoError(t, err)
			if diff := cmp.Diff(tc.want, got); diff != "" { t.Errorf("diff (-want +got):\n%s", diff) }
			if tc.inspect != nil { tc.inspect(t, got) }
		})
	}
}
