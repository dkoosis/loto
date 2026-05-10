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

const (
	testKindGlobal  = "global"
	testParseTagErr = "loto: parse tag"
)

func TestLOTO_ReadGlobalTag_When_TagStateVaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		input           func(t *testing.T, baseDir string, l *loto.LOTO)
		want            *loto.Tag
		wantErr         error
		wantErrContains string
		inspect         func(*testing.T, *loto.Tag)
	}{
		{
			name: "error when global tag does not exist",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
			},
			wantErr: os.ErrNotExist,
		},
		{
			name: "error when global tag has malformed json",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte("{"), 0o600); err != nil {
					t.Fatalf("write malformed tag: %v", err)
				}
			},
			wantErrContains: testParseTagErr,
		},
		{
			name: "boundary error when global tag is empty file",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte(""), 0o600); err != nil {
					t.Fatalf("write empty tag: %v", err)
				}
			},
			wantErrContains: testParseTagErr,
		},
		{
			name: "error when global tag has invalid timestamp type",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(baseDir, "global.tag"), []byte(`{"timestamp":123}`), 0o600); err != nil {
					t.Fatalf("write invalid timestamp tag: %v", err)
				}
			},
			wantErrContains: testParseTagErr,
		},
		{
			name: "happy path reads tag written by global lock",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
				lock, err := l.TryGlobalLock("agent-a", "release")
				if err != nil {
					t.Fatalf("acquire global lock: %v", err)
				}
				t.Cleanup(func() {
					if err := lock.Unlock(); err != nil {
						t.Fatalf("unlock global lock: %v", err)
					}
				})
			},
			want: &loto.Tag{AgentID: "agent-a", Intent: "release", Target: testKindGlobal, Kind: testKindGlobal},
			inspect: func(t *testing.T, got *loto.Tag) {
				t.Helper()
				if got.PID <= 0 {
					t.Fatalf("PID invariant violated: got %d", got.PID)
				}
				if got.Timestamp.IsZero() {
					t.Fatal("timestamp invariant violated: zero timestamp")
				}
				if got.Timestamp.Location() != time.UTC {
					t.Fatalf("timestamp invariant violated: expected UTC, got %v", got.Timestamp.Location())
				}
			},
		},
		{
			name: "TTL option populates ExpiresAt after Timestamp",
			input: func(t *testing.T, baseDir string, l *loto.LOTO) {
				t.Helper()
				lock, err := l.TryGlobalLock("agent-b", "release", loto.TagOptions{TTL: 15 * time.Minute})
				if err != nil {
					t.Fatalf("acquire global lock: %v", err)
				}
				t.Cleanup(func() {
					if err := lock.Unlock(); err != nil {
						t.Fatalf("unlock global lock: %v", err)
					}
				})
			},
			want: &loto.Tag{AgentID: "agent-b", Intent: "release", Target: testKindGlobal, Kind: testKindGlobal},
			inspect: func(t *testing.T, got *loto.Tag) {
				t.Helper()
				if got.ExpiresAt.IsZero() {
					t.Fatal("ExpiresAt invariant violated: zero with TTL set")
				}
				if !got.ExpiresAt.After(got.Timestamp) {
					t.Fatalf("ExpiresAt invariant violated: %v not after Timestamp %v", got.ExpiresAt, got.Timestamp)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			baseDir := filepath.Join(t.TempDir(), "coord")
			l, err := loto.New(baseDir)
			if err != nil {
				t.Fatalf("new loto: %v", err)
			}

			tc.input(t, baseDir, l)

			got, err := l.ReadGlobalTag()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if tc.wantErrContains != "" {
				if err == nil {
					t.Fatal("expected an error but got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("expected error to contain %q, got %q", tc.wantErrContains, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotCmp := *got
			gotCmp.PID = 0
			gotCmp.Timestamp = time.Time{}
			gotCmp.ExpiresAt = time.Time{}
			gotCmp.Host = ""
			gotCmp.Branch = ""
			gotCmp.Cwd = ""
			wantCmp := *tc.want

			if !reflect.DeepEqual(wantCmp, gotCmp) {
				t.Fatalf("tag mismatch: want %+v got %+v", wantCmp, gotCmp)
			}

			if tc.inspect != nil {
				tc.inspect(t, got)
			}
		})
	}
}
