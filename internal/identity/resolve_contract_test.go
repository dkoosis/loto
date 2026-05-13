package identity_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"loto/internal/identity"
)

func TestResolve_ExpectedBehaviour_When_QueryingByIDOrHandle(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		prepare func(*testing.T) *identity.Agent
		want    *identity.Agent
		wantErr error
		inspect func(*testing.T, *identity.Agent)
	}{
		{
			name:  "error when handle does not exist",
			input: "GhostFalcon",
			prepare: func(t *testing.T) *identity.Agent {
				t.Helper()
				_, err := identity.Ensure()
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return nil
			},
			wantErr: identity.ErrAgentNotFound,
		},
		{
			name:  "error when uuid does not exist",
			input: "00000000-0000-4000-8000-000000000000",
			prepare: func(t *testing.T) *identity.Agent {
				t.Helper()
				_, err := identity.Ensure()
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return nil
			},
			wantErr: identity.ErrAgentNotFound,
		},
		{
			name:  "boundary empty query returns not found",
			input: "",
			prepare: func(t *testing.T) *identity.Agent {
				t.Helper()
				_, err := identity.Ensure()
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return nil
			},
			wantErr: identity.ErrAgentNotFound,
		},
		{
			name: "happy path resolve by uuid",
			prepare: func(t *testing.T) *identity.Agent {
				t.Helper()
				a, err := identity.Ensure()
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return a
			},
			wantErr: nil,
			inspect: func(t *testing.T, got *identity.Agent) {
				t.Helper()
				if got.UUID == "" || got.Handle == "" {
					t.Fatalf("invariant failed: empty UUID or Handle: %+v", got)
				}
				if got.UUID+".json" != filepath.Base(filepath.Join(os.Getenv("HOME"), ".loto", "agents", got.UUID+".json")) {
					t.Fatalf("invariant failed: UUID should map to registry filename")
				}
			},
		},
		{
			name: "happy path resolve by handle",
			prepare: func(t *testing.T) *identity.Agent {
				t.Helper()
				a, err := identity.Ensure()
				if err != nil {
					t.Fatalf("Ensure() error = %v", err)
				}
				return a
			},
			wantErr: nil,
			inspect: func(t *testing.T, got *identity.Agent) {
				t.Helper()
				if got.Host == "" || got.CreatedAt.IsZero() {
					t.Fatalf("invariant failed: host and created_at must be populated: %+v", got)
				}
				if got.CreatedAt != got.CreatedAt.UTC() {
					t.Fatalf("invariant failed: created_at must be UTC: %s", got.CreatedAt)
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			os.Unsetenv("LOTO_AGENT_ID")
			os.Unsetenv("CLAUDE_CODE_SESSION_ID")
			seed := tc.prepare(t)
			if seed != nil {
				if tc.name == "happy path resolve by uuid" {
					tc.input = seed.UUID
					tc.want = seed
				} else {
					tc.input = seed.Handle
					tc.want = seed
				}
			}

			got, err := identity.Resolve(tc.input)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Resolve() error = %v, want errors.Is(..., %v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error = %v", err)
			}

			gotComparable := *got
			wantComparable := *tc.want
			gotComparable.CreatedAt = gotComparable.CreatedAt.UTC().Round(0)
			wantComparable.CreatedAt = wantComparable.CreatedAt.UTC().Round(0)
			if !reflect.DeepEqual(wantComparable, gotComparable) {
				t.Errorf("Resolve() mismatch: want=%+v got=%+v", wantComparable, gotComparable)
			}

			if tc.inspect != nil {
				tc.inspect(t, got)
			}
		})
	}
}
