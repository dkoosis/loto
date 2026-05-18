package identity_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"loto/internal/identity"
)

type requireFns struct{}

var require requireFns

func (requireFns) NoError(t *testing.T, err error) { t.Helper(); if err != nil { t.Fatalf("expected no error, got %v", err) } }
func (requireFns) Error(t *testing.T, err error)   { t.Helper(); if err == nil { t.Fatalf("expected error, got nil") } }
func (requireFns) ErrorIs(t *testing.T, err, target error) {
	t.Helper(); if !errors.Is(err, target) { t.Fatalf("expected error %v, got %v", target, err) }
}
func (requireFns) Equal(t *testing.T, want, got any) { t.Helper(); if !reflect.DeepEqual(want, got) { t.Fatalf("want %v got %v", want, got) } }
func (requireFns) NotEmpty(t *testing.T, s string)   { t.Helper(); if s == "" { t.Fatalf("expected non-empty") } }
func (requireFns) True(t *testing.T, ok bool)        { t.Helper(); if !ok { t.Fatalf("expected true") } }
func (requireFns) False(t *testing.T, ok bool, msg string, args ...any) {
	t.Helper(); if ok { t.Fatalf(msg, args...) }
}
func (requireFns) Regexp(t *testing.T, re *regexp.Regexp, s string) {
	t.Helper(); if !re.MatchString(s) { t.Fatalf("%q does not match %s", s, re.String()) }
}

func TestLookupByUUID_ExpectedBehaviour_When_RecordResolution(t *testing.T) {

	tests := []struct {
		name      string
		setup     func(t *testing.T, home string) string
		want      *identity.Agent
		wantErr   error
		syntaxErr bool
		inspect   func(*testing.T, *identity.Agent)
	}{
		{name: "error when uuid record does not exist", setup: func(t *testing.T, _ string) string { return "missing-uuid" }, wantErr: os.ErrNotExist},
		{name: "error when uuid is empty boundary", setup: func(t *testing.T, _ string) string { return "" }, wantErr: os.ErrNotExist},
		{name: "error when record contains malformed json", syntaxErr: true, setup: func(t *testing.T, home string) string {
			uuid := "bad-json"; path := filepath.Join(home, ".loto", "agents", uuid+".json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700)); require.NoError(t, os.WriteFile(path, []byte("{"), 0o600)); return uuid
		}},
		{name: "success resolves persisted record", setup: func(t *testing.T, home string) string {
			uuid := "agent-123"; a := identity.Agent{UUID: uuid, Handle: "SwiftFalcon", Host: "devbox", CreatedAt: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)}
			body, err := json.Marshal(a); require.NoError(t, err)
			path := filepath.Join(home, ".loto", "agents", uuid+".json"); require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700)); require.NoError(t, os.WriteFile(path, body, 0o600)); return uuid
		}, want: &identity.Agent{UUID: "agent-123", Handle: "SwiftFalcon", Host: "devbox", CreatedAt: time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)}, inspect: func(t *testing.T, got *identity.Agent) {
			require.Equal(t, "agent-123", got.UUID); require.NotEmpty(t, got.Handle); require.True(t, got.CreatedAt.Equal(time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)))
		}},
		{name: "success ignores non-target sibling files", setup: func(t *testing.T, home string) string {
			require.NoError(t, os.MkdirAll(filepath.Join(home, ".loto", "agents"), 0o700)); require.NoError(t, os.WriteFile(filepath.Join(home, ".loto", "agents", "notes.txt"), []byte("ignore me"), 0o600))
			uuid := "agent-789"; a := identity.Agent{UUID: uuid, Handle: "BoldOtter", Host: "devbox", CreatedAt: time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC)}
			body, err := json.Marshal(a); require.NoError(t, err); require.NoError(t, os.WriteFile(filepath.Join(home, ".loto", "agents", uuid+".json"), body, 0o600)); return uuid
		}, want: &identity.Agent{UUID: "agent-789", Handle: "BoldOtter", Host: "devbox", CreatedAt: time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC)}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir(); t.Setenv("HOME", home); uuid := tc.setup(t, home)
			got, err := identity.LookupByUUID(uuid)
			if tc.wantErr != nil || tc.syntaxErr {
				require.Error(t, err)
				if tc.syntaxErr { var se *json.SyntaxError; require.True(t, errors.As(err, &se)) } else { require.ErrorIs(t, err, tc.wantErr) }
				return
			}
			require.NoError(t, err)
			require.True(t, got != nil)
			require.Equal(t, tc.want.UUID, got.UUID)
			require.Equal(t, tc.want.Handle, got.Handle)
			require.Equal(t, tc.want.Host, got.Host)
			require.True(t, tc.want.CreatedAt.Equal(got.CreatedAt))
			if tc.inspect != nil { tc.inspect(t, got) }
		})
	}
}

func TestNewUUID_ExpectedBehaviour_When_GeneratingValues(t *testing.T) {

	tests := []struct {
		name    string
		input   int
		want    int
		wantErr error
		inspect func(*testing.T, []string)
	}{
		{
			name:  "happy path generates requested number of unique v4 uuids",
			input: 8,
			want:  8,
			inspect: func(t *testing.T, got []string) {
				seen := make(map[string]struct{}, len(got))
				re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
				for _, u := range got {
					require.Regexp(t, re, u)
					_, exists := seen[u]
					require.False(t, exists, "uuid duplicated: %s", u)
					seen[u] = struct{}{}
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := make([]string, 0, tc.input)
			for range tc.input {
				got = append(got, identity.NewUUID())
			}

			require.NoError(t, nil)
			require.Equal(t, tc.want, len(got))
			if tc.inspect != nil {
				tc.inspect(t, got)
			}
		})
	}
}
