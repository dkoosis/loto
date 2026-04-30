package loto_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"loto"
)

func TestReadGlobalTag_ExpectedBehaviour_When_TagStateChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   func(*testing.T, string, *loto.LOTO)
		want    *loto.Tag
		wantErr error
		inspect func(*testing.T, *loto.Tag)
	}{
		{
			name: "error when global tag is missing",
			input: func(t *testing.T, _ string, _ *loto.LOTO) {
				t.Helper()
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "error when global tag JSON is malformed",
			input: func(t *testing.T, baseDir string, _ *loto.LOTO) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte("{"), 0o600))
			},
			wantErr: errors.New("loto: parse tag"),
		},
		{
			name: "boundary error when global tag is empty",
			input: func(t *testing.T, baseDir string, _ *loto.LOTO) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte(""), 0o600))
			},
			wantErr: errors.New("loto: parse tag"),
		},
		{
			name: "error when timestamp has wrong type",
			input: func(t *testing.T, baseDir string, _ *loto.LOTO) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte(`{"timestamp":123}`), 0o600))
			},
			wantErr: errors.New("loto: parse tag"),
		},
		{
			name: "happy path returns lock holder tag",
			input: func(t *testing.T, _ string, l *loto.LOTO) {
				t.Helper()
				lock, err := l.TryGlobalLock("agent-a", "release", loto.TagOptions{TTL: 15 * time.Minute})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
			},
			want: &loto.Tag{AgentID: "agent-a", Intent: "release", Target: "global", Kind: "global"},
			inspect: func(t *testing.T, got *loto.Tag) {
				t.Helper()
				require.Greater(t, got.PID, 0)
				require.False(t, got.Timestamp.IsZero())
				require.Equal(t, time.UTC, got.Timestamp.Location())
				require.False(t, got.ExpiresAt.IsZero())
				require.True(t, got.ExpiresAt.After(got.Timestamp))
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			baseDir := filepath.Join(t.TempDir(), "coord")
			l, err := loto.New(baseDir)
			require.NoError(t, err)

			tc.input(t, baseDir, l)

			got, err := l.ReadGlobalTag()

			if tc.wantErr != nil {
				if tc.wantErr == os.ErrNotExist {
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					require.Error(t, err)
					require.ErrorContains(t, err, tc.wantErr.Error())
				}
				return
			}
			require.NoError(t, err)

			gotCmp := *got
			gotCmp.PID = 0
			gotCmp.Host = ""
			gotCmp.Branch = ""
			gotCmp.Cwd = ""
			gotCmp.Timestamp = time.Time{}
			gotCmp.ExpiresAt = time.Time{}
			if !reflect.DeepEqual(tc.want, &gotCmp) {
				t.Errorf("want %+v got %+v", *tc.want, gotCmp)
			}

			if tc.inspect != nil {
				tc.inspect(t, got)
			}
		})
	}
}

type testRequire struct{}

var require testRequire

func (testRequire) NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
func (testRequire) Error(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
}
func (testRequire) ErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v, got %v", target, err)
	}
}
func (testRequire) ErrorContains(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("expected error containing %q, got %v", contains, err)
	}
}
func (testRequire) Greater(t *testing.T, got, min int) {
	t.Helper()
	if got <= min {
		t.Fatalf("expected %d > %d", got, min)
	}
}
func (testRequire) False(t *testing.T, v bool) {
	t.Helper()
	if v {
		t.Fatal("expected false")
	}
}
func (testRequire) True(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Fatal("expected true")
	}
}
func (testRequire) Equal(t *testing.T, want, got interface{}) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %v got %v", want, got)
	}
}
